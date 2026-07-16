package wireguard

import (
	"context"
	"testing"
)

// fakeIdentityStore is an in-memory IdentityStore.
type fakeIdentityStore struct {
	key PrivateKey
	ok  bool
}

func (f *fakeIdentityStore) WireGuardPrivateKey(context.Context) (PrivateKey, bool, error) {
	return f.key, f.ok, nil
}

func (f *fakeIdentityStore) SetWireGuardPrivateKey(_ context.Context, key PrivateKey) error {
	f.key = key
	f.ok = true
	return nil
}

func TestLoadOrGenerateIdentityGeneratesOnFirstCall(t *testing.T) {
	store := &fakeIdentityStore{}

	priv, err := LoadOrGenerateIdentity(context.Background(), store)
	if err != nil {
		t.Fatalf("LoadOrGenerateIdentity: %v", err)
	}
	if priv == (PrivateKey{}) {
		t.Error("LoadOrGenerateIdentity returned the zero key")
	}
	if !store.ok {
		t.Error("LoadOrGenerateIdentity did not persist the generated key")
	}
	if store.key != priv {
		t.Errorf("persisted key %s != returned key %s", store.key.Base64(), priv.Base64())
	}
}

func TestLoadOrGenerateIdentityReusesPersistedKey(t *testing.T) {
	store := &fakeIdentityStore{}

	first, err := LoadOrGenerateIdentity(context.Background(), store)
	if err != nil {
		t.Fatalf("first LoadOrGenerateIdentity: %v", err)
	}
	second, err := LoadOrGenerateIdentity(context.Background(), store)
	if err != nil {
		t.Fatalf("second LoadOrGenerateIdentity: %v", err)
	}
	if first != second {
		t.Errorf("identity changed across calls: %s != %s", first.Base64(), second.Base64())
	}
}
