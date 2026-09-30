package configsync

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// designatedYAML is a fleet that passes config.Validate — which a Clone runs
// and Syncer doesn't — with a Designation naming its one instance.
// (fixtureYAML's static_ip sits outside its own dhcp_excluded_range.)
const designatedYAML = `
kind: Network
name: dev-lan
cidr: 10.0.0.0/24
gateway: 10.0.0.1
dhcp_excluded_range: 10.0.0.200-10.0.0.250
---
kind: Instance
name: devnode0
mac: aa:bb:cc:dd:ee:00
network: dev-lan
static_ip: 10.0.0.210
disk: single
nic: single
applications: [incus]
---
kind: Designation
primary: devnode0
`

func newClone(t *testing.T, remote string, threshold int) *Clone {
	t.Helper()
	return &Clone{RepoURL: remote, Ref: DefaultRef, Dir: filepath.Join(t.TempDir(), "clone"), FailureThreshold: threshold}
}

func mustSync(t *testing.T, c *Clone) Snapshot {
	t.Helper()
	snap, err := c.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return snap
}

func mustHave(t *testing.T, c *Clone, commit plumbing.Hash, want bool) {
	t.Helper()
	got, err := c.HasCommit(commit.String())
	if err != nil {
		t.Fatalf("HasCommit(%s): %v", commit, err)
	}
	if got != want {
		t.Errorf("HasCommit(%s) = %v, want %v", commit, got, want)
	}
}

func TestCloneSyncsAndParsesDesignation(t *testing.T) {
	src := t.TempDir()
	repo := initFixtureRepo(t, src)
	head := commitFixtureFile(t, repo, src, "fleet.yaml", designatedYAML)

	c := newClone(t, src, 3)
	snap := mustSync(t, c)
	if snap.Commit != head.String() {
		t.Errorf("Commit = %s, want %s", snap.Commit, head)
	}
	if got := snap.Config.Primary(); got != "devnode0" {
		t.Errorf("Primary() = %q, want devnode0", got)
	}
	if cur, err := c.Current(); err != nil || cur.Commit != head.String() {
		t.Errorf("Current() = %v, %v", cur.Commit, err)
	}
	mustHave(t, c, head, true)
}

// The clone is persistent: a later Sync fetches into it, and a restarted
// agent (a new Clone over the same Dir) reuses it.
func TestCloneFetchesIntoAPersistentClone(t *testing.T) {
	src := t.TempDir()
	repo := initFixtureRepo(t, src)
	first := commitFixtureFile(t, repo, src, "fleet.yaml", designatedYAML)

	c := newClone(t, src, 3)
	mustSync(t, c)
	ancestors := reflect.ValueOf(c.ancestors).Pointer()
	mustSync(t, c) // nothing new: already up to date is not a failure
	if reflect.ValueOf(c.ancestors).Pointer() != ancestors {
		t.Error("an unmoved branch should reuse the cached snapshot, not re-walk its history")
	}

	second := commitFixtureFile(t, repo, src, "extra.yaml", "kind: Designation\nprimary: devnode0\n")
	if _, err := c.Sync(context.Background()); err == nil {
		t.Fatal("two Designations must fail validation")
	}
	third := commitFixtureFile(t, repo, src, "extra.yaml", "# nothing\n")
	if snap := mustSync(t, c); snap.Commit != third.String() {
		t.Fatalf("Commit = %s, want %s", snap.Commit, third)
	}
	for _, h := range []plumbing.Hash{first, second, third} {
		mustHave(t, c, h, true)
	}

	restarted := &Clone{RepoURL: src, Ref: DefaultRef, Dir: c.Dir, FailureThreshold: 3}
	if snap := mustSync(t, restarted); snap.Commit != third.String() {
		t.Errorf("restarted Commit = %s, want %s", snap.Commit, third)
	}
	mustHave(t, restarted, first, true)
}

// A force-pushed branch is followed, not refused, and the abandoned commits
// are no longer in this agent's history even though the clone still holds
// them.
func TestCloneFollowsAForcePush(t *testing.T) {
	src := t.TempDir()
	repo := initFixtureRepo(t, src)
	base := commitFixtureFile(t, repo, src, "fleet.yaml", designatedYAML)
	abandoned := commitFixtureFile(t, repo, src, "notes.yaml", "# old history\n")

	c := newClone(t, src, 3)
	if snap := mustSync(t, c); snap.Commit != abandoned.String() {
		t.Fatalf("Commit = %s, want %s", snap.Commit, abandoned)
	}

	// Rewrite main: reset to base and commit something else on top.
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Reset(&git.ResetOptions{Commit: base, Mode: git.HardReset}); err != nil {
		t.Fatal(err)
	}
	rewritten := commitFixtureFile(t, repo, src, "notes.yaml", "# new history\n")

	snap := mustSync(t, c)
	if snap.Commit != rewritten.String() {
		t.Fatalf("after force-push Commit = %s, want %s", snap.Commit, rewritten)
	}
	mustHave(t, c, rewritten, true)
	mustHave(t, c, base, true)
	mustHave(t, c, abandoned, false)
}

// #187: Current keeps serving the last good snapshot through transient
// failures, then errors once FailureThreshold consecutive syncs have failed,
// and recovers on the next success.
func TestCloneFailureThreshold(t *testing.T) {
	src := t.TempDir()
	repo := initFixtureRepo(t, src)
	good := commitFixtureFile(t, repo, src, "fleet.yaml", designatedYAML)

	c := newClone(t, src, 3)
	if _, err := c.Current(); err == nil {
		t.Fatal("Current before any sync must error")
	}
	mustSync(t, c)

	c.RepoURL = filepath.Join(t.TempDir(), "gone") // the remote becomes unreachable
	for i := 1; i <= 3; i++ {
		if _, err := c.Sync(context.Background()); err == nil {
			t.Fatalf("sync %d against a missing remote succeeded", i)
		}
		cur, err := c.Current()
		switch {
		case i < 3 && (err != nil || cur.Commit != good.String()):
			t.Fatalf("after %d failures Current() = %q, %v; want the last good commit", i, cur.Commit, err)
		case i == 3 && err == nil:
			t.Fatalf("after %d failures Current() must error", i)
		}
		if c.Failing() != (i >= 3) {
			t.Errorf("after %d failures Failing() = %v", i, c.Failing())
		}
	}

	c.RepoURL = src // the remote is back; the clone is repointed
	mustSync(t, c)
	if _, err := c.Current(); err != nil || c.Failing() {
		t.Errorf("after recovery: Current err=%v Failing=%v", err, c.Failing())
	}
}

// A commit that doesn't validate is a failed sync: the previous snapshot, and
// its history, stay current.
func TestCloneKeepsLastGoodOnAnInvalidCommit(t *testing.T) {
	src := t.TempDir()
	repo := initFixtureRepo(t, src)
	good := commitFixtureFile(t, repo, src, "fleet.yaml", designatedYAML)
	c := newClone(t, src, 5)
	mustSync(t, c)

	bad := commitFixtureFile(t, repo, src, "fleet.yaml", strings.Replace(designatedYAML, "primary: devnode0", "primary: nosuchnode", 1))
	_, err := c.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nosuchnode") {
		t.Fatalf("Sync = %v, want a validation error naming the unknown instance", err)
	}
	cur, err := c.Current()
	if err != nil || cur.Commit != good.String() {
		t.Errorf("Current() = %q, %v; want the last good commit", cur.Commit, err)
	}
	// The agent's history is its last good commit's, so a peer that accepted
	// the newer commit reads as ahead of it.
	mustHave(t, c, bad, false)
}

func TestCloneHasCommitRejectsNonHashes(t *testing.T) {
	c := newClone(t, t.TempDir(), 1)
	for _, s := range []string{"", "abc", "zz" + strings.Repeat("0", 38), strings.Repeat("0", 41)} {
		if got, err := c.HasCommit(s); got || err != nil {
			t.Errorf("HasCommit(%q) = %v, %v", s, got, err)
		}
	}
}

func TestCloneRequiresDir(t *testing.T) {
	c := &Clone{RepoURL: t.TempDir()}
	if _, err := c.Sync(context.Background()); err == nil {
		t.Error("Sync without Dir must error")
	}
}
