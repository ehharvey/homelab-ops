package secureboot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dirNames lists dir's entries, so a test can tell a temporary file was left.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteFilesWritesModesAndRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	key, cert := filepath.Join(dir, "a.key"), filepath.Join(dir, "a.crt")
	// A key left group-readable by something else is tightened on replace.
	if err := os.WriteFile(key, []byte("old"), 0o644); err != nil { //nolint:gosec // G306: the loose mode is what the test replaces
		t.Fatal(err)
	}
	files := []File{{Path: key, Data: []byte("key"), Private: true}, {Path: cert, Data: []byte("cert")}}

	err := WriteFiles(files, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("without force over an existing file: got %v, want an 'already exists' error", err)
	}
	if _, err := os.Stat(cert); err == nil {
		t.Error("a refused WriteFiles wrote the other file anyway")
	}

	if err := WriteFiles(files, true); err != nil {
		t.Fatalf("with force: %v", err)
	}
	for path, want := range map[string]struct {
		data string
		mode os.FileMode
	}{key: {"key", 0o600}, cert: {"cert", 0o644}} {
		got, err := os.ReadFile(path) //nolint:gosec // G304: a test temp dir
		if err != nil || string(got) != want.data {
			t.Errorf("%s = %q (%v), want %q", path, got, err, want.data)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want.mode {
			t.Errorf("%s mode %v, want %v", path, st.Mode().Perm(), want.mode)
		}
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"a.crt", "a.key"}) {
		t.Errorf("dir holds %v, want only the two targets", names)
	}
}

func TestWriteFilesLeavesEveryTargetAsItWasWhenAWriteFails(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	for _, p := range []string{first, second} {
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A 250-byte name is a valid target, but its temporary file's name
	// (".<name>.tmp-<n>") is past the 255-byte limit, so writing the third
	// file fails after the first two have been staged.
	long := filepath.Join(dir, strings.Repeat("x", 250))
	err := WriteFiles([]File{
		{Path: first, Data: []byte("new")},
		{Path: second, Data: []byte("new")},
		{Path: long, Data: []byte("new")},
	}, true)
	if err == nil {
		t.Fatal("want an error from the unwritable third file")
	}
	for _, p := range []string{first, second} {
		if got, err := os.ReadFile(p); err != nil || string(got) != "old" { //nolint:gosec // G304: a test temp dir
			t.Errorf("%s = %q (%v) after a failed WriteFiles, want it untouched", filepath.Base(p), got, err)
		}
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"first", "second"}) {
		t.Errorf("dir holds %v after a failed WriteFiles, want no temporary files", names)
	}
}

func TestWriteFilesRefusesToReplaceANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	plain, sub := filepath.Join(dir, "plain"), filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	err := WriteFiles([]File{{Path: plain, Data: []byte("x")}, {Path: sub, Data: []byte("x")}}, true)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("got %v, want a 'not a regular file' error", err)
	}
	if _, err := os.Stat(plain); err == nil {
		t.Error("the other target was written although WriteFiles refused")
	}
}
