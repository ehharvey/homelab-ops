package vault

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/sealed"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func key(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := sealed.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return id
}

func fleet() config.Config {
	return config.Config{
		Networks: []config.Network{{Name: "lan", CIDR: netip.MustParsePrefix("10.0.0.0/24")}},
		Instances: []config.Instance{{
			Name: "node0", MAC: "aa:bb:cc:dd:ee:ff", Network: "lan",
			StaticIP: netip.MustParseAddr("10.0.0.20"), Disk: "/dev/sda", NIC: "eth0",
		}},
	}
}

var syncedAt = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// newFileVault is a vault over a fresh store file path in a temp dir.
func newFileVault(t *testing.T) (*Vault, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "web.db")
	v, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, path
}

// reopen is the vault a restart gets over the same file.
func reopen(t *testing.T, path string) *Vault {
	t.Helper()
	v, err := New(path)
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func mustSync(t *testing.T, v *Vault, commit string) {
	t.Helper()
	if err := v.Replace(context.Background(), fleet(), commit, syncedAt); err != nil {
		t.Fatalf("Replace: %v", err)
	}
}

func wantCommit(t *testing.T, v *Vault, want string) {
	t.Helper()
	commit, _, ok, err := v.LastSync(context.Background())
	if err != nil || !ok || commit != want {
		t.Errorf("LastSync = %q ok:%v err:%v, want %q", commit, ok, err, want)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a test temp path
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	return b
}

// Every store call is refused while the vault isn't unlocked.
func TestLockedRefusesEveryStoreCall(t *testing.T) {
	v, _ := newFileVault(t)
	ctx := context.Background()
	calls := map[string]func() error{
		"Replace":           func() error { return v.Replace(ctx, fleet(), "c", syncedAt) },
		"LastSync":          func() error { _, _, _, err := v.LastSync(ctx); return err },
		"Networks":          func() error { _, err := v.Networks(ctx); return err },
		"Network":           func() error { _, _, err := v.Network(ctx, "lan"); return err },
		"Instances":         func() error { _, err := v.Instances(ctx); return err },
		"Instance":          func() error { _, _, err := v.Instance(ctx, "node0"); return err },
		"InstanceTunnelIPs": func() error { _, err := v.InstanceTunnelIPs(ctx); return err },
		"Apps":              func() error { _, err := v.Apps(ctx); return err },
		"InstanceCredential": func() error {
			_, _, err := v.InstanceCredential(ctx, "node0")
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrLocked) {
			t.Errorf("%s on a vault with no store = %v, want ErrLocked", name, err)
		}
	}
}

func TestInitThenRestartIsLockedUntilUnlocked(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	if s := v.State(); s != NoStore {
		t.Fatalf("State before init = %s, want %s", s, NoStore)
	}
	k := key(t)
	if err := v.Init(ctx, k); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if s := v.State(); s != Unlocked {
		t.Fatalf("State after init = %s, want %s", s, Unlocked)
	}
	mustSync(t, v, "abc")

	// On disk: age ciphertext, not sqlite, and no other key opens it.
	ct := readFile(t, path)
	if !sealed.IsSealed(ct) || bytes.Contains(ct, []byte("SQLite format 3")) {
		t.Fatalf("the store file isn't age ciphertext: %q", ct[:min(len(ct), 32)])
	}
	if _, err := sealed.Open(ct, key(t)); !errors.Is(err, sealed.ErrWrongKey) {
		t.Errorf("another key opened the store file: %v", err)
	}
	if _, err := sealed.Open(ct, k); err != nil {
		t.Errorf("the store's own key didn't open its file: %v", err)
	}

	// A restart is locked, and init is refused there.
	r := reopen(t, path)
	if s := r.State(); s != Locked {
		t.Fatalf("State after restart = %s, want %s", s, Locked)
	}
	if err := r.Init(ctx, key(t)); !errors.Is(err, ErrState) {
		t.Errorf("Init on a locked store = %v, want ErrState", err)
	}
	if err := r.Unlock(ctx, k); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantCommit(t, r, "abc")
	if n, ok, err := r.Network(ctx, "lan"); err != nil || !ok || n.CIDR.String() != "10.0.0.0/24" {
		t.Errorf("Network after unlock = %+v ok:%v err:%v, want the synced lan", n, ok, err)
	}
	if err := r.Unlock(ctx, k); !errors.Is(err, ErrState) {
		t.Errorf("a second Unlock = %v, want ErrState", err)
	}
}

func TestUnlockWithWrongKeyWritesNothing(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	if err := v.Init(ctx, key(t)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")
	before := readFile(t, path)

	r := reopen(t, path)
	if err := r.Unlock(ctx, key(t)); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("Unlock with another key = %v, want ErrWrongKey", err)
	}
	if s := r.State(); s != Locked {
		t.Errorf("State after a wrong key = %s, want %s", s, Locked)
	}
	if after := readFile(t, path); !bytes.Equal(before, after) {
		t.Error("a wrong-key unlock changed the store file's bytes")
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("the store directory holds %d entries after a wrong-key unlock, want 1", len(entries))
	}
}

func TestRotateKeepsStateAndRetiresTheOldKey(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	old, next := key(t), key(t)
	if err := v.Init(ctx, old); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")

	if err := v.Rotate(ctx, key(t), next); !errors.Is(err, ErrWrongKey) {
		t.Errorf("Rotate with an old key that isn't the store's = %v, want ErrWrongKey", err)
	}
	if err := v.Rotate(ctx, old, next); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	wantCommit(t, v, "abc")
	// A write after the rotate is sealed to the new key too.
	mustSync(t, v, "def")

	r := reopen(t, path)
	if err := r.Unlock(ctx, old); !errors.Is(err, ErrWrongKey) {
		t.Errorf("Unlock with the rotated-out key = %v, want ErrWrongKey", err)
	}
	if err := r.Unlock(ctx, next); err != nil {
		t.Fatalf("Unlock with the new key: %v", err)
	}
	wantCommit(t, r, "def")

	if err := reopen(t, path).Rotate(ctx, next, key(t)); !errors.Is(err, ErrState) {
		t.Errorf("Rotate on a locked store = %v, want ErrState", err)
	}
}

func TestResetMovesTheOldFileAsideAndStartsEmpty(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	lost := key(t)
	if err := v.Init(ctx, lost); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")
	old := readFile(t, path)

	if _, err := v.Reset(ctx, key(t)); !errors.Is(err, ErrState) {
		t.Errorf("Reset on an unlocked store = %v, want ErrState", err)
	}

	r := reopen(t, path)
	next := key(t)
	movedTo, err := r.Reset(ctx, next)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if aside := readFile(t, movedTo); !bytes.Equal(aside, old) {
		t.Errorf("the file moved aside to %s isn't the old store", movedTo)
	}
	if _, _, ok, err := r.LastSync(ctx); err != nil || ok {
		t.Errorf("LastSync after reset = ok:%v err:%v, want an empty store", ok, err)
	}
	if _, err := sealed.Open(readFile(t, path), next); err != nil {
		t.Errorf("the new store file doesn't open with the new key: %v", err)
	}
	// The old key still opens what was moved aside.
	if _, err := sealed.Open(readFile(t, movedTo), lost); err != nil {
		t.Errorf("the moved-aside file no longer opens with its key: %v", err)
	}
}

func TestInMemoryStartsEmptyAndLockedEveryTime(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{":memory:", ""} {
		v, err := New(path)
		if err != nil {
			t.Fatalf("New(%q): %v", path, err)
		}
		if v.Persistent() || v.State() != NoStore {
			t.Fatalf("New(%q): persistent %v, state %s; want in memory, %s", path, v.Persistent(), v.State(), NoStore)
		}
		if _, _, _, err := v.LastSync(ctx); !errors.Is(err, ErrLocked) {
			t.Errorf("LastSync before init = %v, want ErrLocked", err)
		}
		if err := v.Unlock(ctx, key(t)); !errors.Is(err, ErrState) {
			t.Errorf("Unlock of an in-memory store = %v, want ErrState", err)
		}
		if _, err := v.Reset(ctx, key(t)); !errors.Is(err, ErrState) {
			t.Errorf("Reset of an in-memory store = %v, want ErrState", err)
		}
		k := key(t)
		if err := v.Init(ctx, k); err != nil {
			t.Fatalf("Init: %v", err)
		}
		mustSync(t, v, "abc")
		wantCommit(t, v, "abc")
		if err := v.Rotate(ctx, k, key(t)); err != nil {
			t.Errorf("Rotate of an in-memory store: %v", err)
		}
		_ = v.Close()
	}
}

// No migration: a plaintext store file from before #197 stops startup.
func TestNewRefusesAPlaintextStoreFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.db")
	if err := os.WriteFile(path, []byte("SQLite format 3\x00..."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); !errors.Is(err, sealed.ErrNotSealed) {
		t.Errorf("New over a plaintext store = %v, want ErrNotSealed", err)
	}
}

// An empty file or a directory at STORE_PATH is named as what it is, not
// as a plaintext store.
func TestNewNamesAnEmptyFileOrADirectory(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{empty: "empty file", dir: "is a directory"} {
		_, err := New(path)
		if err == nil || errors.Is(err, sealed.ErrNotSealed) || !strings.Contains(err.Error(), want) {
			t.Errorf("New(%s) = %v, want an error saying %q", path, err, want)
		}
	}
}

// A failed write undoes the change in memory: a later read doesn't see it,
// and the next write doesn't carry it to disk.
func TestAFailedWriteUndoesTheChange(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	id := key(t)
	if err := v.Init(ctx, id); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")

	full := errors.New("no space left on device")
	v.writeFile = func(string, []byte) error { return full }
	cred := nodeprovision.Credential{WireGuardPrivateKey: wireguard.PrivateKey{1}}
	if err := v.SetInstanceCredential(ctx, "node0", cred); !errors.Is(err, full) {
		t.Fatalf("SetInstanceCredential with the write failing = %v, want %v", err, full)
	}
	if _, ok, err := v.InstanceCredential(ctx, "node0"); err != nil || ok {
		t.Errorf("InstanceCredential after the failed write = ok:%v err:%v, want none", ok, err)
	}
	if err := v.Replace(ctx, fleet(), "def", syncedAt); !errors.Is(err, full) {
		t.Fatalf("Replace with the write failing = %v, want %v", err, full)
	}
	wantCommit(t, v, "abc")

	v.writeFile = sealed.WriteFileAtomic
	mustSync(t, v, "ghi")
	r := reopen(t, path)
	if err := r.Unlock(ctx, id); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	wantCommit(t, r, "ghi")
	if _, ok, err := r.InstanceCredential(ctx, "node0"); err != nil || ok {
		t.Errorf("the undone credential reached the file: ok:%v err:%v", ok, err)
	}
}

// A symlinked STORE_PATH, dangling until init, stays a link: the store is
// its target, which a restart unlocks through the link.
func TestASymlinkedStorePathKeepsTheLink(t *testing.T) {
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "web.db")
	link := filepath.Join(t.TempDir(), "web.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	v, err := New(link)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	if s := v.State(); s != NoStore {
		t.Fatalf("a dangling link's state = %s, want %s", s, NoStore)
	}
	id := key(t)
	if err := v.Init(ctx, id); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("STORE_PATH is no longer a link: %v, %v", info, err)
	}
	if !sealed.IsSealed(readFile(t, target)) {
		t.Fatal("the link's target isn't the sealed store")
	}
	r := reopen(t, link)
	if err := r.Unlock(ctx, id); err != nil {
		t.Fatalf("Unlock through the link: %v", err)
	}
	wantCommit(t, r, "abc")
}

// Extra recipients (#509's seam) can open the file too.
func TestExtraRecipientsCanOpenTheFile(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	recovery := key(t)
	v.Extra = []age.Recipient{recovery.Recipient()}
	if err := v.Init(ctx, key(t)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")
	if err := reopen(t, path).Unlock(ctx, recovery); err != nil {
		t.Errorf("Unlock with the extra recipient's identity: %v", err)
	}
}
