// Package scripts holds tests for the shell scripts in this directory, so
// they run in `make test` and CI's build-test job. It has no non-test code.
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
)

// vendoredFile renders a file the way scripts/vendor-incusos.sh does: the
// stamp on the first line, then the attribution header, then the body.
func vendoredFile(sha, body string) string {
	return "// Vendored from github.com/lxc/incus-os @ " + sha + "\n" +
		"// (incus-osd/api/seed/install.go), Apache-2.0 license — see third_party/incus-os/COPYING.\n" +
		"// Unmodified.\n" +
		"// Regenerate via scripts/vendor-incusos.sh; do not hand-edit beyond what\n" +
		"// that script does.\n\n" + body
}

const (
	installBody = "package seed\n\ntype Install struct {\n\tForceInstall bool `json:\"force_install\"`\n}\n"
	networkBody = "package seed\n\ntype Network struct {\n\tName string `json:\"name\"`\n}\n"
)

// fixtureRepo is a throwaway repo shaped like this one after a bump: a
// baseline commit with the submodule at upstream commit 1 and files vendored
// from shaA, ready for a case to make its working-tree changes.
type fixtureRepo struct {
	t   *testing.T
	dir string
}

func newFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	r := &fixtureRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")

	// The submodule is a nested repo with two commits, so moving its HEAD
	// changes the gitlink exactly as `git -C third_party/incus-os checkout
	// <tag>` does in the workflow.
	sub := filepath.Join("third_party", "incus-os")
	r.write(filepath.Join(sub, "README"), "v1\n")
	r.gitIn(sub, "init", "-q", "-b", "main")
	r.gitIn(sub, "add", ".")
	r.gitIn(sub, "commit", "-q", "-m", "v1")
	r.write(filepath.Join(sub, "README"), "v2\n")
	r.gitIn(sub, "commit", "-q", "-am", "v2")
	r.gitIn(sub, "checkout", "-q", "HEAD~1")

	r.write("go.mod", "module example.com/fixture\n")
	r.write("internal/third_party/incusos/api/seed/install.go", vendoredFile(shaA, installBody))
	r.write("internal/third_party/incusos/api/seed/network.go", vendoredFile(shaA, networkBody))
	r.git("add", ".")
	r.git("commit", "-q", "-m", "baseline")
	return r
}

func (r *fixtureRepo) gitIn(sub string, args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "advice.addEmbeddedRepo=false"}, args...)...)
	cmd.Dir = filepath.Join(r.dir, sub)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *fixtureRepo) git(args ...string) { r.t.Helper(); r.gitIn("", args...) }

func (r *fixtureRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *fixtureRepo) remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, rel)); err != nil {
		r.t.Fatal(err)
	}
}

// bumpStamps is the provenance-only bump: the submodule moves to upstream
// commit 2 and every vendored file's stamp moves to shaB, nothing else.
func (r *fixtureRepo) bumpStamps() {
	r.t.Helper()
	r.gitIn(filepath.Join("third_party", "incus-os"), "checkout", "-q", "main")
	r.write("internal/third_party/incusos/api/seed/install.go", vendoredFile(shaB, installBody))
	r.write("internal/third_party/incusos/api/seed/network.go", vendoredFile(shaB, networkBody))
}

// gitEnv keeps the operator's and CI's git config out of the fixture repos.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
	)
}

func TestIncusOSBumpIsTrivial(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	script, err := filepath.Abs("incusos-bump-is-trivial.sh")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		change   func(r *fixtureRepo)
		wantExit int
		// want are substrings the report must contain: which path was
		// taken and, on the review path, what changed beyond the stamps.
		want []string
	}{
		{
			name:     "stamps and submodule pointer only",
			change:   (*fixtureRepo).bumpStamps,
			wantExit: 0,
			want:     []string{"Provenance-only bump: auto-merge enabled", "in 2 vendored file(s)"},
		},
		{
			name: "struct field changed",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.write("internal/third_party/incusos/api/seed/install.go", vendoredFile(shaB,
					strings.Replace(installBody, "}\n", "\tTarget string `json:\"target\"`\n}\n", 1)))
			},
			wantExit: 1,
			want: []string{
				"Needs review: auto-merge not enabled",
				"`internal/third_party/incusos/api/seed/install.go (+1 -0)`",
				"+\tTarget string",
			},
		},
		{
			name: "vendored file added",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.write("internal/third_party/incusos/api/seed/storage.go", vendoredFile(shaB, "package seed\n"))
			},
			wantExit: 1,
			want:     []string{"Needs review", "`internal/third_party/incusos/api/seed/storage.go (added)`"},
		},
		{
			name: "vendored file deleted",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.remove("internal/third_party/incusos/api/seed/network.go")
			},
			wantExit: 1,
			want:     []string{"Needs review", "`internal/third_party/incusos/api/seed/network.go (deleted)`"},
		},
		{
			name: "vendored file renamed",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.remove("internal/third_party/incusos/api/seed/network.go")
				r.write("internal/third_party/incusos/api/seed/net.go", vendoredFile(shaB, networkBody))
			},
			wantExit: 1,
			want: []string{
				"`internal/third_party/incusos/api/seed/network.go (deleted)`",
				"`internal/third_party/incusos/api/seed/net.go (added)`",
			},
		},
		{
			name: "change outside the allowed paths",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.write("go.mod", "module example.com/fixture\n\nrequire github.com/lxc/incus/v7 v7.1.0\n")
			},
			wantExit: 1,
			want:     []string{"Needs review", "outside `third_party/incus-os`", "- `go.mod`"},
		},
		{
			name: "new file outside the allowed paths",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.write("go.sum", "github.com/lxc/incus/v7 v7.1.0 h1:x\n")
			},
			wantExit: 1,
			want:     []string{"- `go.sum (added)`"},
		},
		{
			name: "stamp with a malformed sha",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				r.write("internal/third_party/incusos/api/seed/install.go", vendoredFile("v2.0", installBody))
			},
			wantExit: 1,
			want:     []string{"`internal/third_party/incusos/api/seed/install.go (+1 -0)`"},
		},
		{
			name: "vendored file mode changed",
			change: func(r *fixtureRepo) {
				r.bumpStamps()
				p := filepath.Join(r.dir, "internal/third_party/incusos/api/seed/install.go")
				if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // G302: the fixture needs an executable bit to change the file's git mode
					r.t.Fatal(err)
				}
			},
			wantExit: 1,
			want:     []string{"install.go (mode 100644 -> 100755)`"},
		},
		{
			name:     "nothing changed",
			change:   func(*fixtureRepo) {},
			wantExit: 1,
			want:     []string{"No change was found at all"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newFixtureRepo(t)
			tc.change(r)

			cmd := exec.Command(script)
			cmd.Dir = r.dir
			cmd.Env = gitEnv()
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			exit := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			report := string(out)
			if exit != tc.wantExit {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", exit, tc.wantExit, report, stderr.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(report, w) {
					t.Errorf("report lacks %q:\n%s", w, report)
				}
			}
		})
	}
}
