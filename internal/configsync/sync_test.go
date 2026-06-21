package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const fixtureYAML = `
kind: Network
name: dev-lan
cidr: 10.0.0.0/24
gateway: 10.0.0.1
dhcp_excluded_range: 10.0.0.200-10.0.0.250
dns: [10.0.0.1]
---
kind: Instance
name: devnode0
mac: aa:bb:cc:dd:ee:00
network: dev-lan
static_ip: 10.0.0.10
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
`

func initFixtureRepo(t *testing.T, dir string) *git.Repository {
	t.Helper()

	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{
			DefaultBranch: plumbing.NewBranchReferenceName(DefaultRef),
		},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions: %v", err)
	}
	commitFixtureFile(t, repo, dir, "fleet.yaml", fixtureYAML)
	return repo
}

func commitFixtureFile(t *testing.T, repo *git.Repository, dir, name, content string) plumbing.Hash {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatalf("Add: %v", err)
	}
	hash, err := wt.Commit("fixture commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return hash
}

func TestSyncInitialClone(t *testing.T) {
	dir := t.TempDir()
	initFixtureRepo(t, dir)

	s := &Syncer{RepoURL: dir, Ref: DefaultRef}
	cfg, sha, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if sha == "" {
		t.Error("Sync returned empty commit SHA")
	}
	if len(cfg.Networks) != 1 || cfg.Networks[0].Name != "dev-lan" {
		t.Errorf("Networks = %+v, want one Network named dev-lan", cfg.Networks)
	}
	if len(cfg.Instances) != 1 || cfg.Instances[0].Name != "devnode0" {
		t.Errorf("Instances = %+v, want one Instance named devnode0", cfg.Instances)
	}
}

func TestSyncPicksUpNewCommit(t *testing.T) {
	dir := t.TempDir()
	repo := initFixtureRepo(t, dir)

	s := &Syncer{RepoURL: dir, Ref: DefaultRef}
	_, firstSHA, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (first): %v", err)
	}

	const updatedYAML = fixtureYAML + `
---
kind: Network
name: second-lan
cidr: 10.1.0.0/24
gateway: 10.1.0.1
dhcp_excluded_range: 10.1.0.200-10.1.0.250
dns: [10.1.0.1]
`
	commitFixtureFile(t, repo, dir, "fleet.yaml", updatedYAML)

	cfg, secondSHA, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync (second): %v", err)
	}

	if secondSHA == firstSHA {
		t.Error("second Sync returned the same commit SHA as the first")
	}
	if len(cfg.Networks) != 2 {
		t.Errorf("Networks = %+v, want 2", cfg.Networks)
	}
}

func TestSyncErrors(t *testing.T) {
	t.Run("bad repo URL", func(t *testing.T) {
		s := &Syncer{RepoURL: filepath.Join(t.TempDir(), "does-not-exist"), Ref: DefaultRef}
		if _, _, err := s.Sync(context.Background()); err == nil {
			t.Error("Sync with a nonexistent repo URL: got nil error")
		}
	})

	t.Run("bad ref", func(t *testing.T) {
		dir := t.TempDir()
		initFixtureRepo(t, dir)

		s := &Syncer{RepoURL: dir, Ref: "does-not-exist"}
		if _, _, err := s.Sync(context.Background()); err == nil {
			t.Error("Sync with a nonexistent ref: got nil error")
		}
	})
}
