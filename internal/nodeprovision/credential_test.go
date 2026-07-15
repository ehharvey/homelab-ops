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

func (f *fakeCredentialStore) SetInstanceCredential(_ context.Context, name string, cred Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creds == nil {
		f.creds = make(map[string]Credential)
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
