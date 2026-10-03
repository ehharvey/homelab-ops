package configsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// remoteName is the one remote a Clone fetches from.
const remoteName = "origin"

// Snapshot is one successfully synced, validated commit of the config repo.
type Snapshot struct {
	Config config.Config
	// Commit is the full SHA the Config was read at.
	Commit string
}

// Clone is the agent's view of the config repo: a persistent, full-history
// bare clone on disk that each Sync fetches into, rather than Syncer's fresh
// in-memory depth-1 clone.
//
// It keeps history because leader election needs it (docs/Decisions.md §25):
// an agent may act only if every peer's published commit is in its own
// history, which HasCommit answers locally. A config repo is small, so the
// whole history costs little.
//
// It keeps the last good Snapshot, and Current serves it until Sync has failed
// FailureThreshold times in a row (#187). After that Current errors, which
// leaderelection treats as "not leader": an agent cut off from git stops
// acting on a designation it can no longer confirm.
//
// A Sync succeeds only if the fetched commit parses and passes
// config.Validate. Anything else — an unreachable remote, a missing branch, a
// commit that doesn't parse or validate — is a failed sync, and the previous
// Snapshot stays current (and its commit is what the agent publishes). Unlike
// the web app's Syncer, validation happens here, since "last good" has to mean
// usable.
//
// Safe for concurrent use.
type Clone struct {
	// RepoURL is the git remote (any go-git-supported transport).
	RepoURL string
	// Ref is the branch to follow. Defaults to DefaultRef.
	Ref string
	// Dir is where the bare clone lives. It is created on first use and
	// reused across restarts; a Dir holding some other repository has its
	// origin remote repointed at RepoURL.
	Dir string
	// FailureThreshold is how many consecutive failed syncs Current
	// tolerates before it errors. Values below 1 mean 1.
	FailureThreshold int

	mu        sync.Mutex
	repo      *git.Repository
	last      *Snapshot
	ancestors map[plumbing.Hash]bool
	failures  int
	lastErr   error
}

// Sync fetches Ref (force-updating the local tracking ref, so a force-pushed
// branch is followed rather than refused), then parses and validates the
// fetched commit. On success it becomes Current and its history is what
// HasCommit consults.
func (c *Clone) Sync(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	snap, ancestors, err := c.sync(ctx)
	if err != nil {
		c.failures++
		c.lastErr = err
		return Snapshot{}, err
	}
	c.failures, c.lastErr = 0, nil
	c.last, c.ancestors = &snap, ancestors
	return snap, nil
}

// Current returns the last good Snapshot, or an error if there has been none
// yet or the last FailureThreshold syncs all failed.
func (c *Clone) Current() (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		if c.lastErr != nil {
			return Snapshot{}, fmt.Errorf("configsync: no successful sync yet: %w", c.lastErr)
		}
		return Snapshot{}, errors.New("configsync: no successful sync yet")
	}
	if c.failures >= c.threshold() {
		return Snapshot{}, fmt.Errorf("configsync: %d consecutive syncs failed: %w", c.failures, c.lastErr)
	}
	return *c.last, nil
}

// Failing reports whether Current is erroring because syncs keep failing:
// at least FailureThreshold consecutive failures.
func (c *Clone) Failing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures >= c.threshold()
}

// HasCommit reports whether commit is in the history of the current
// Snapshot's commit: that commit or one of its ancestors.
//
// That's deliberately narrower than "the object is somewhere in the clone".
// After a force-push the clone still holds the abandoned commits, but a peer
// still on one of them isn't on this agent's history, and the rule "stand
// down when a peer is on a commit outside my history" is what makes both sides
// of a force-push pause until they converge, rather than one of them trusting
// a commit its branch no longer contains. A string that isn't a full SHA is
// never in the history.
func (c *Clone) HasCommit(commit string) (bool, error) {
	if len(commit) != 40 || !plumbing.IsHash(commit) {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ancestors[plumbing.NewHash(commit)], nil
}

func (c *Clone) threshold() int {
	if c.FailureThreshold < 1 {
		return 1
	}
	return c.FailureThreshold
}

func (c *Clone) ref() string {
	if c.Ref == "" {
		return DefaultRef
	}
	return c.Ref
}

func (c *Clone) sync(ctx context.Context) (Snapshot, map[plumbing.Hash]bool, error) {
	repo, err := c.open()
	if err != nil {
		return Snapshot{}, nil, err
	}

	branch := c.ref()
	tracking := plumbing.NewRemoteReferenceName(remoteName, branch)
	// The leading "+" is what lets a fetch move the tracking ref to a commit
	// that doesn't descend from the old one. Without it go-git (v5.19)
	// doesn't even error on a force-pushed branch: the fetch "succeeds" and
	// silently leaves the tracking ref on the abandoned commit, so the agent
	// would sit on a stale checkout forever. TestCloneFollowsAForcePush pins
	// this.
	spec := gitconfig.RefSpec(fmt.Sprintf("+%s:%s", plumbing.NewBranchReferenceName(branch), tracking))
	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   []gitconfig.RefSpec{spec},
		Tags:       git.NoTags,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return Snapshot{}, nil, fmt.Errorf("fetch %s (ref %s): %w", c.RepoURL, branch, err)
	}

	ref, err := repo.Reference(tracking, true)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("resolve %s: %w", tracking, err)
	}
	if c.last != nil && ref.Hash().String() == c.last.Commit {
		// Most polls: the branch hasn't moved. Same commit, same config and
		// history, so skip the re-parse, re-validate and history walk.
		return *c.last, c.ancestors, nil
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("read commit %s: %w", ref.Hash(), err)
	}
	cfg, err := parseCommit(commit)
	if err != nil {
		return Snapshot{}, nil, fmt.Errorf("commit %s: %w", ref.Hash(), err)
	}
	if issues := config.Validate(cfg); !issues.Empty() {
		return Snapshot{}, nil, fmt.Errorf("commit %s: validate: %w", ref.Hash(), issues)
	}

	ancestors := map[plumbing.Hash]bool{}
	iter := object.NewCommitPreorderIter(commit, nil, nil)
	defer iter.Close()
	if err := iter.ForEach(func(cm *object.Commit) error {
		ancestors[cm.Hash] = true
		return nil
	}); err != nil {
		return Snapshot{}, nil, fmt.Errorf("walk history of %s: %w", ref.Hash(), err)
	}
	return Snapshot{Config: cfg, Commit: ref.Hash().String()}, ancestors, nil
}

// open returns the bare repository in Dir, creating it on first use, with
// its origin remote pointing at RepoURL.
func (c *Clone) open() (*git.Repository, error) {
	if c.repo != nil {
		return c.repo, c.ensureRemote(c.repo)
	}
	if c.Dir == "" {
		return nil, errors.New("configsync: Clone.Dir is required")
	}
	repo, err := git.PlainOpen(c.Dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInit(c.Dir, true)
	}
	if err != nil {
		return nil, fmt.Errorf("open clone at %s: %w", c.Dir, err)
	}
	if err := c.ensureRemote(repo); err != nil {
		return nil, err
	}
	c.repo = repo
	return repo, nil
}

func (c *Clone) ensureRemote(repo *git.Repository) error {
	remote, err := repo.Remote(remoteName)
	switch {
	case errors.Is(err, git.ErrRemoteNotFound):
	case err != nil:
		return fmt.Errorf("read remote %s: %w", remoteName, err)
	case len(remote.Config().URLs) == 1 && remote.Config().URLs[0] == c.RepoURL:
		return nil
	default:
		if err := repo.DeleteRemote(remoteName); err != nil {
			return fmt.Errorf("repoint remote %s: %w", remoteName, err)
		}
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: remoteName, URLs: []string{c.RepoURL}}); err != nil {
		return fmt.Errorf("create remote %s: %w", remoteName, err)
	}
	return nil
}

// parseCommit parses every *.yaml/*.yml regular file at the root of commit's
// tree, in file-name order, into one Config — the same repo layout Syncer
// reads (docs/Config Schema.md § The repo), read from git objects rather than
// a checked-out worktree, since a bare clone has none.
func parseCommit(commit *object.Commit) (config.Config, error) {
	tree, err := commit.Tree()
	if err != nil {
		return config.Config{}, fmt.Errorf("read tree: %w", err)
	}
	entries := append([]object.TreeEntry(nil), tree.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	var cfg config.Config
	for _, e := range entries {
		if !e.Mode.IsFile() || e.Mode == filemode.Symlink || !isConfigFile(e.Name) {
			continue
		}
		f, err := tree.TreeEntryFile(&e)
		if err != nil {
			return config.Config{}, fmt.Errorf("open %s: %w", e.Name, err)
		}
		r, err := f.Reader()
		if err != nil {
			return config.Config{}, fmt.Errorf("open %s: %w", e.Name, err)
		}
		parsed, err := parseAndClose(r)
		if err != nil {
			return config.Config{}, fmt.Errorf("parse %s: %w", e.Name, err)
		}
		cfg.Append(parsed)
	}
	return cfg, nil
}

func parseAndClose(r io.ReadCloser) (config.Config, error) {
	parsed, err := config.Parse(r)
	return parsed, errors.Join(err, r.Close())
}

func isConfigFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
