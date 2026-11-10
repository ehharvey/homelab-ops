package vault

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/sealed"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// initFileVault is an unlocked vault over a fresh store file, synced once,
// whose writes are counted.
func initFileVault(t *testing.T) (v *Vault, path string, writes *atomic.Int32) {
	t.Helper()
	v, path = newFileVault(t)
	if err := v.Init(context.Background(), key(t)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")
	writes = new(atomic.Int32)
	v.writeFile = func(path string, data []byte) error {
		writes.Add(1)
		return sealed.WriteFileAtomic(path, data)
	}
	return v, path, writes
}

// A read while a change is being written waits for the write, so it never
// sees a change that a failed write then undoes: a credential read there
// could be flashed into an image though the file never took it.
func TestAReadWaitsForAChangesWrite(t *testing.T) {
	ctx := context.Background()
	v, _, _ := initFileVault(t)
	entered, release := make(chan struct{}), make(chan struct{})
	full := errors.New("no space left on device")
	v.writeFile = func(string, []byte) error {
		close(entered)
		<-release
		return full
	}
	setErr := make(chan error, 1)
	go func() {
		setErr <- v.SetInstanceCredential(ctx, "node0", nodeprovision.Credential{WireGuardPrivateKey: wireguard.PrivateKey{1}})
	}()
	<-entered

	type read struct {
		ok  bool
		err error
	}
	got := make(chan read, 1)
	go func() {
		_, ok, err := v.InstanceCredential(ctx, "node0")
		got <- read{ok, err}
	}()
	select {
	case r := <-got:
		close(release)
		t.Fatalf("InstanceCredential returned (ok:%v err:%v) while the credential's write was still in progress", r.ok, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-setErr; !errors.Is(err, full) {
		t.Fatalf("SetInstanceCredential = %v, want %v", err, full)
	}
	if r := <-got; r.err != nil || r.ok {
		t.Errorf("InstanceCredential after the failed write = ok:%v err:%v, want none", r.ok, r.err)
	}
}

// A reset whose new store can't be written moves the old file back, so the
// store is left locked as it was, not moved aside with nothing in its place.
func TestAFailedResetPutsTheOldStoreBack(t *testing.T) {
	ctx := context.Background()
	v, path := newFileVault(t)
	if err := v.Init(ctx, key(t)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mustSync(t, v, "abc")
	old := readFile(t, path)

	r := reopen(t, path)
	full := errors.New("no space left on device")
	r.writeFile = func(string, []byte) error { return full }
	movedTo, err := r.Reset(ctx, key(t))
	if !errors.Is(err, full) {
		t.Fatalf("Reset with the write failing = %v, want %v", err, full)
	}
	if movedTo != "" {
		t.Errorf("Reset reported the old store moved to %q, though it was put back", movedTo)
	}
	if s := r.State(); s != Locked {
		t.Errorf("State after the failed reset = %s, want %s", s, Locked)
	}
	if !bytes.Equal(readFile(t, path), old) {
		t.Error("the store file after the failed reset isn't the old store")
	}
}

// A reset whose caller goes away after the old file is moved still
// finishes, rather than leaving no store.
func TestAResetOutlastsItsCallersCancellation(t *testing.T) {
	v, path := newFileVault(t)
	if err := v.Init(context.Background(), key(t)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	r := reopen(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	next := key(t)
	if _, err := r.Reset(ctx, next); err != nil {
		t.Fatalf("Reset with a cancelled context: %v", err)
	}
	if s := r.State(); s != Unlocked {
		t.Errorf("State after the reset = %s, want %s", s, Unlocked)
	}
	if _, err := sealed.Open(readFile(t, path), next); err != nil {
		t.Errorf("the new store file doesn't open with the new key: %v", err)
	}
}

// A sync that moved only the time isn't written; a new commit is. The
// time is kept in memory until the next write takes it.
func TestReplaceWritesOnlyWhenTheSnapshotChanged(t *testing.T) {
	ctx := context.Background()
	v, path, writes := initFileVault(t)
	id := v.id
	later := syncedAt.Add(time.Hour)
	if err := v.Replace(ctx, fleet(), "abc", later); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if n := writes.Load(); n != 0 {
		t.Errorf("an unchanged sync wrote the store file %d times, want 0", n)
	}
	if _, at, _, err := v.LastSync(ctx); err != nil || !at.Equal(later) {
		t.Errorf("LastSync's time = %v (err %v), want %v in memory", at, err, later)
	}
	r := reopen(t, path)
	if err := r.Unlock(ctx, id); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, at, _, err := r.LastSync(ctx); err != nil || !at.Equal(syncedAt) {
		t.Errorf("LastSync's time after a restart = %v (err %v), want the last written %v", at, err, syncedAt)
	}

	changed := fleet()
	changed.Instances[0].Disk = "/dev/sdb"
	for _, c := range []struct {
		what string
		cfg  func() error
	}{
		{"a new commit", func() error { return v.Replace(ctx, fleet(), "def", later) }},
		{"a changed fleet", func() error { return v.Replace(ctx, changed, "def", later) }},
	} {
		before := writes.Load()
		if err := c.cfg(); err != nil {
			t.Fatalf("Replace with %s: %v", c.what, err)
		}
		if n := writes.Load() - before; n != 1 {
			t.Errorf("Replace with %s wrote the store file %d times, want 1", c.what, n)
		}
	}
}

// A batch of credentials is written once, and only if one was set; a failed
// write undoes them all.
func TestBatchCredentialsWritesOnce(t *testing.T) {
	ctx := context.Background()
	v, _, writes := initFileVault(t)
	names := []string{"node0", "node1", "node2"}
	if err := v.BatchCredentials(ctx, func(cs nodeprovision.CredentialStore) {
		for _, n := range names {
			if _, err := nodeprovision.EnsureCredential(ctx, cs, n); err != nil {
				t.Errorf("EnsureCredential(%s): %v", n, err)
			}
		}
	}); err != nil {
		t.Fatalf("BatchCredentials: %v", err)
	}
	if n := writes.Load(); n != 1 {
		t.Errorf("minting %d credentials in a batch wrote the store file %d times, want 1", len(names), n)
	}
	for _, n := range names {
		if _, ok, err := v.InstanceCredential(ctx, n); err != nil || !ok {
			t.Errorf("InstanceCredential(%s) after the batch = ok:%v err:%v", n, ok, err)
		}
	}

	if err := v.BatchCredentials(ctx, func(cs nodeprovision.CredentialStore) {
		for _, n := range names {
			if _, err := nodeprovision.EnsureCredential(ctx, cs, n); err != nil {
				t.Errorf("EnsureCredential(%s): %v", n, err)
			}
		}
	}); err != nil {
		t.Fatalf("BatchCredentials with nothing to mint: %v", err)
	}
	if n := writes.Load(); n != 1 {
		t.Errorf("a batch that minted nothing wrote the store file; %d writes, want 1", n)
	}

	full := errors.New("no space left on device")
	v.writeFile = func(string, []byte) error { return full }
	err := v.BatchCredentials(ctx, func(cs nodeprovision.CredentialStore) {
		for _, n := range []string{"node3", "node4"} {
			if _, err := nodeprovision.EnsureCredential(ctx, cs, n); err != nil {
				t.Errorf("EnsureCredential(%s): %v", n, err)
			}
		}
	})
	if !errors.Is(err, full) {
		t.Fatalf("BatchCredentials with the write failing = %v, want %v", err, full)
	}
	for _, n := range []string{"node3", "node4"} {
		if _, ok, err := v.InstanceCredential(ctx, n); err != nil || ok {
			t.Errorf("InstanceCredential(%s) after the failed batch = ok:%v err:%v, want none", n, ok, err)
		}
	}
}
