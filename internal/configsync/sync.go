// Package configsync pulls a git repo and parses its k8s-style fleet YAML
// (see internal/config) into a Config, without requiring a git binary on
// the host — clone/fetch happens entirely in-process via go-git.
package configsync

import (
	"context"
	"fmt"
	"strings"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

// DefaultRef is used when Syncer.Ref is unset.
const DefaultRef = "main"

// Syncer pulls one branch of one git repo and parses its fleet YAML.
type Syncer struct {
	// RepoURL is the git remote to clone (any go-git-supported transport:
	// git://, http(s)://, or a local filesystem path).
	RepoURL string
	// Ref is the branch to clone. Defaults to DefaultRef.
	Ref string
}

// Sync clones RepoURL at Ref and parses every *.yaml/*.yml file at the
// repo root into a config.Config, returning the resolved commit SHA.
func (s *Syncer) Sync(ctx context.Context) (config.Config, string, error) {
	ref := s.Ref
	if ref == "" {
		ref = DefaultRef
	}

	fs := memfs.New()
	repo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:           s.RepoURL,
		ReferenceName: plumbing.NewBranchReferenceName(ref),
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return config.Config{}, "", fmt.Errorf("clone %s (ref %s): %w", s.RepoURL, ref, err)
	}

	head, err := repo.Head()
	if err != nil {
		return config.Config{}, "", fmt.Errorf("resolve HEAD: %w", err)
	}

	entries, err := fs.ReadDir("/")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("read repo root: %w", err)
	}

	var cfg config.Config
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}

		f, err := fs.Open(name)
		if err != nil {
			return config.Config{}, "", fmt.Errorf("open %s: %w", name, err)
		}
		parsed, parseErr := config.Parse(f)
		closeErr := f.Close()
		if parseErr != nil {
			return config.Config{}, "", fmt.Errorf("parse %s: %w", name, parseErr)
		}
		if closeErr != nil {
			return config.Config{}, "", fmt.Errorf("close %s: %w", name, closeErr)
		}

		cfg.Networks = append(cfg.Networks, parsed.Networks...)
		cfg.Instances = append(cfg.Instances, parsed.Instances...)
	}

	return cfg, head.Hash().String(), nil
}
