package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// examplesDir is the repo's examples/ directory, relative to this package.
const examplesDir = "../../examples"

// TestExamplesParseAndValidate reads each examples/<name>/ directory the way
// internal/configsync reads a config repo — every *.yaml/*.yml file at its
// root, in file-name order, merged — and requires the result to parse and pass
// Validate. It exists so the examples can't drift from the schema they
// document: a renamed field or a new required one fails here, not in an
// operator's first sync.
func TestExamplesParseAndValidate(t *testing.T) {
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("read %s: %v", examplesDir, err)
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		t.Fatalf("no example directories under %s", examplesDir)
	}

	for _, name := range dirs {
		t.Run(name, func(t *testing.T) {
			cfg := parseRepoRoot(t, filepath.Join(examplesDir, name))
			if len(cfg.Networks)+len(cfg.Instances)+len(cfg.Apps)+len(cfg.Designations) == 0 {
				t.Fatalf("no documents parsed; an example must declare something")
			}
			if issues := Validate(cfg); !issues.Empty() {
				t.Errorf("Validate: %v", issues)
			}
		})
	}
}

// parseRepoRoot mirrors configsync.Syncer.Sync's file handling on a plain
// directory: root-level *.yaml/*.yml only, sorted by name, subdirectories
// ignored.
func parseRepoRoot(t *testing.T, dir string) Config {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var cfg Config
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || (!strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml")) {
			continue
		}
		f, err := os.Open(filepath.Join(dir, n)) //nolint:gosec // G304: test reads the repo's own examples/
		if err != nil {
			t.Fatalf("open %s: %v", n, err)
		}
		parsed, err := Parse(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		cfg.Append(parsed)
	}
	return cfg
}
