package nodeprovision

import (
	"context"
	"sync"
	"testing"
)

// fakeCredentialStore is an in-memory CredentialStore.
type fakeCredentialStore struct {
	mu    sync.Mutex
	creds map[string]Credential
}

func (f *fakeCredentialStore) InstanceCredential(_ context.Context, name string) (Credential, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[name]
	return c, ok, nil
}

// SetInstanceCredential mirrors internal/store's real ON CONFLICT DO
// NOTHING semantics (insert once per name, silently discard a second
// write) — EnsureCredential's concurrency safety depends on this, so a
// fake implementing "last write wins" instead would let a fixed race
// slip through a concurrency test undetected.
func (f *fakeCredentialStore) SetInstanceCredential(_ context.Context, name string, cred Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creds == nil {
		f.creds = make(map[string]Credential)
	}
	if _, exists := f.creds[name]; exists {
		return nil
	}
	f.creds[name] = cred
	return nil
}

func TestEnsureCredentialMintsOnFirstCall(t *testing.T) {
	store := &fakeCredentialStore{}

	cred, err := EnsureCredential(context.Background(), store, "node0")
	if err != nil {
		t.Fatalf("EnsureCredential: %v", err)
	}
	if cred.WireGuardPrivateKey == ([32]byte{}) {
		t.Error("EnsureCredential produced a zero WireGuard private key")
	}
	if len(cred.BootstrapCertPEM) == 0 || len(cred.BootstrapKeyPEM) == 0 {
		t.Error("EnsureCredential produced an empty bootstrap cert/key")
	}

	stored, ok, err := store.InstanceCredential(context.Background(), "node0")
	if err != nil || !ok {
		t.Fatalf("InstanceCredential(node0) = ok:%v err:%v, want a persisted credential", ok, err)
	}
	if stored.WireGuardPrivateKey != cred.WireGuardPrivateKey {
		t.Error("persisted credential's WireGuard key does not match the one EnsureCredential returned")
	}
}

func TestEnsureCredentialNeverRegenerates(t *testing.T) {
	store := &fakeCredentialStore{}

	first, err := EnsureCredential(context.Background(), store, "node0")
	if err != nil {
		t.Fatalf("first EnsureCredential: %v", err)
	}
	second, err := EnsureCredential(context.Background(), store, "node0")
	if err != nil {
		t.Fatalf("second EnsureCredential: %v", err)
	}

	if first.WireGuardPrivateKey != second.WireGuardPrivateKey {
		t.Error("EnsureCredential regenerated the WireGuard key on a second call for the same instance")
	}
	if string(first.BootstrapCertPEM) != string(second.BootstrapCertPEM) {
		t.Error("EnsureCredential regenerated the bootstrap cert on a second call for the same instance")
	}
}

func TestEnsureCredentialDistinctPerInstance(t *testing.T) {
	store := &fakeCredentialStore{}

	a, err := EnsureCredential(context.Background(), store, "node-a")
	if err != nil {
		t.Fatalf("EnsureCredential(node-a): %v", err)
	}
	b, err := EnsureCredential(context.Background(), store, "node-b")
	if err != nil {
		t.Fatalf("EnsureCredential(node-b): %v", err)
	}

	if a.WireGuardPrivateKey == b.WireGuardPrivateKey {
		t.Error("two different instances got the same WireGuard private key")
	}
}

// TestEnsureCredentialConcurrentCallsConverge proves the fix for the
// check-then-act race: EnsureCredential's read-then-mint-then-write isn't
// atomic on its own, and it's called from two paths with no shared lock
// between them in production (a manual seed/image fetch can race the
// background sync poller's reconcile pass for the same brand-new
// instance). Without SetInstanceCredential's insert-once semantics (see
// its doc comment) and the re-read afterward, concurrent callers could
// each mint a *different* keypair and race to persist theirs, leaving
// whichever one already got returned to its caller permanently mismatched
// with whatever ended up in the store.
func TestEnsureCredentialConcurrentCallsConverge(t *testing.T) {
	store := &fakeCredentialStore{}
	const n = 50

	results := make([]Credential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			results[i], errs[i] = EnsureCredential(context.Background(), store, "racing-node")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureCredential call %d: %v", i, err)
		}
	}
	want := results[0]
	for i, got := range results {
		if got.WireGuardPrivateKey != want.WireGuardPrivateKey {
			t.Errorf("call %d returned WireGuard key %s, want %s (all concurrent callers must converge on one credential)",
				i, got.WireGuardPrivateKey.Base64(), want.WireGuardPrivateKey.Base64())
		}
	}
}
