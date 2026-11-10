package sealed

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func mustKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return id
}

func TestSealOpenRoundTrips(t *testing.T) {
	id := mustKey(t)
	plain := []byte("SQLite format 3\x00 and the rest of a store")
	ct, err := Seal(plain, id.Recipient())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !IsSealed(ct) || bytes.Contains(ct, plain) {
		t.Fatalf("ciphertext isn't age, or carries the plaintext: %q", ct[:min(len(ct), 64)])
	}
	got, err := Open(ct, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("Open = %q, want %q", got, plain)
	}
}

func TestOpenRefusesAnotherKey(t *testing.T) {
	ct, err := Seal([]byte("state"), mustKey(t).Recipient())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(ct, mustKey(t)); !errors.Is(err, ErrWrongKey) {
		t.Errorf("Open with another key = %v, want ErrWrongKey", err)
	}
}

// The recipient list is #509's seam: either recipient's identity opens it.
func TestSealToSeveralRecipients(t *testing.T) {
	a, b := mustKey(t), mustKey(t)
	ct, err := Seal([]byte("state"), a.Recipient(), b.Recipient())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for _, id := range []*age.X25519Identity{a, b} {
		if _, err := Open(ct, id); err != nil {
			t.Errorf("Open with recipient %s: %v", id.Recipient(), err)
		}
	}
}

func TestOpenRefusesPlaintextAndTampering(t *testing.T) {
	id := mustKey(t)
	if _, err := Open([]byte("SQLite format 3\x00"), id); !errors.Is(err, ErrNotSealed) {
		t.Errorf("Open(plaintext) = %v, want ErrNotSealed", err)
	}
	ct, err := Seal(bytes.Repeat([]byte("x"), 1000), id.Recipient())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(ct[:len(ct)-10], id); err == nil {
		t.Error("Open(truncated) = nil error, want one")
	}
	flipped := bytes.Clone(ct)
	flipped[len(flipped)-5] ^= 1
	if _, err := Open(flipped, id); err == nil {
		t.Error("Open(altered) = nil error, want one")
	}
}

func TestWriteNewKeyFileRoundTripsAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.key")
	id, err := WriteNewKeyFile(path)
	if err != nil {
		t.Fatalf("WriteNewKeyFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode %04o, want 0600", perm)
	}
	got, err := ReadKeyFile(path)
	if err != nil {
		t.Fatalf("ReadKeyFile: %v", err)
	}
	if got.String() != id.String() {
		t.Error("ReadKeyFile read a different key than WriteNewKeyFile wrote")
	}
	if _, err := WriteNewKeyFile(path); err == nil {
		t.Error("WriteNewKeyFile over an existing file = nil error, want a refusal")
	}
	if again, _ := ReadKeyFile(path); again == nil || again.String() != id.String() {
		t.Error("a refused WriteNewKeyFile changed the existing key file")
	}
}

func TestParseKeyRefusesJunkWithoutEchoingIt(t *testing.T) {
	for _, s := range []string{"", "# only a comment\n", "AGE-SECRET-KEY-1NOTAKEY", "a\nb\n"} {
		_, err := ParseKey(s)
		if err == nil {
			t.Errorf("ParseKey(%q) = nil error, want one", s)
			continue
		}
		if s != "" && strings.Contains(err.Error(), "NOTAKEY") {
			t.Errorf("ParseKey's error echoes its input: %v", err)
		}
	}
}

func TestWriteFileAtomicReplacesWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	for _, data := range []string{"first", "second, longer"} {
		if err := WriteFileAtomic(path, []byte(data)); err != nil {
			t.Fatalf("WriteFileAtomic: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatalf("file = %q, %v; want %q", got, err, data)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only the file (no temp left behind)", len(entries))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode %04o, want 0600", perm)
	}
}

// A symlinked path is written through: the link stays a link, and its
// target, existing or not yet, holds the data.
func TestWriteFileAtomicFollowsASymlink(t *testing.T) {
	for _, dangling := range []bool{false, true} {
		dir, volume := t.TempDir(), t.TempDir()
		target := filepath.Join(volume, "store.db")
		if !dangling {
			if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		link := filepath.Join(dir, "store.db")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := WriteFileAtomic(link, []byte("new")); err != nil {
			t.Fatalf("WriteFileAtomic (dangling %v): %v", dangling, err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("dangling %v: the link was replaced (%v, %v)", dangling, info, err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
			t.Errorf("dangling %v: the target holds %q, %v; want \"new\"", dangling, got, err)
		}
	}
}

func TestResolveLinksFollowsRelativeLinksAndStopsAtALoop(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("b", filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("c", filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveLinks(filepath.Join(dir, "a")); err != nil || got != filepath.Join(dir, "c") {
		t.Errorf("ResolveLinks(a) = %q, %v; want %q", got, err, filepath.Join(dir, "c"))
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveLinks(filepath.Join(dir, "loop")); err == nil {
		t.Error("ResolveLinks(a link to itself) = nil error, want one")
	}
}
