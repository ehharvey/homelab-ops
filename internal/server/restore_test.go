package server

import (
	"context"
	"errors"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// operatorPeer is a fresh operator device at the first operator address.
func operatorPeer(t *testing.T) wireguard.OperatorPeer {
	t.Helper()
	_, pub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	return wireguard.OperatorPeer{PublicKey: pub, Addr: addr("10.100.0.240")}
}

// TestRestoreTunnelPeersRegistersStoredInstances: at startup, with no sync
// run since, every stored instance's peer is on the tunnel under its
// credential's key, and the operators follow (#269).
func TestRestoreTunnelPeersRegistersStoredInstances(t *testing.T) {
	op := operatorPeer(t)
	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{op}}
	creds := &fakeCredentialStore{}
	st := &fakeStore{instances: []config.Instance{
		{Name: "node-a", TunnelIP: addr("10.100.0.2")},
		{Name: "node-b", TunnelIP: addr("10.100.0.3")},
	}}
	svc := NewService(nil, st, nil, nil, tunnels, creds)

	if err := svc.RestoreTunnelPeers(context.Background()); err != nil {
		t.Fatalf("RestoreTunnelPeers: %v", err)
	}

	if st.replaced {
		t.Error("RestoreTunnelPeers replaced the stored snapshot; it must only read it")
	}
	if len(tunnels.upsertCall) != 3 {
		t.Fatalf("len(upsertCall) = %d, want 3 (two stored instances, then the operator)", len(tunnels.upsertCall))
	}
	for i, inst := range st.instances {
		cred, ok, err := creds.InstanceCredential(context.Background(), inst.Name)
		if err != nil || !ok {
			t.Fatalf("credential for %q = ok:%v err:%v, want minted", inst.Name, ok, err)
		}
		call := tunnels.upsertCall[i]
		if want := wireguard.PublicKeyOf(cred.WireGuardPrivateKey); call.pub != want || call.tunnelIP != inst.TunnelIP {
			t.Errorf("upsertCall[%d] = %s at %s, want %s at %s", i, call.pub, call.tunnelIP, want, inst.TunnelIP)
		}
	}
	if last := tunnels.upsertCall[2]; last.pub != op.PublicKey || last.tunnelIP != op.Addr {
		t.Errorf("last upsert = %s at %s, want the operator %s at %s", last.pub, last.tunnelIP, op.PublicKey, op.Addr)
	}
}

// TestRestoreTunnelPeersReusesStoredCredential: the restored peer's key is
// the one already stored for the instance, the key its flashed seed trusts,
// not a fresh one.
func TestRestoreTunnelPeersReusesStoredCredential(t *testing.T) {
	tunnels := &fakeTunnelSource{}
	creds := &fakeCredentialStore{}
	// Mint the credential once, as the sync that preceded the restart did.
	reconcileTunnelPeers(context.Background(), &fakeTunnelSource{}, creds, []config.Instance{{Name: "node-a", TunnelIP: addr("10.100.0.2")}})
	cred, _, _ := creds.InstanceCredential(context.Background(), "node-a")

	st := &fakeStore{instances: []config.Instance{{Name: "node-a", TunnelIP: addr("10.100.0.2")}}}
	if err := NewService(nil, st, nil, nil, tunnels, creds).RestoreTunnelPeers(context.Background()); err != nil {
		t.Fatalf("RestoreTunnelPeers: %v", err)
	}

	if len(tunnels.upsertCall) != 1 || tunnels.upsertCall[0].pub != wireguard.PublicKeyOf(cred.WireGuardPrivateKey) {
		t.Errorf("upsertCall = %+v, want node-a under its stored key %s", tunnels.upsertCall, wireguard.PublicKeyOf(cred.WireGuardPrivateKey))
	}
}

// TestRestoreTunnelPeersEmptyStore: with nothing synced yet, only the
// operators are (re-)registered.
func TestRestoreTunnelPeersEmptyStore(t *testing.T) {
	op := operatorPeer(t)
	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{op}}

	if err := NewService(nil, &fakeStore{}, nil, nil, tunnels, &fakeCredentialStore{}).RestoreTunnelPeers(context.Background()); err != nil {
		t.Fatalf("RestoreTunnelPeers: %v", err)
	}

	if len(tunnels.upsertCall) != 1 || tunnels.upsertCall[0].pub != op.PublicKey {
		t.Errorf("upsertCall = %+v, want only the operator", tunnels.upsertCall)
	}
}

// TestRestoreTunnelPeersUnconfigured: no tunnel (WIREGUARD_ENDPOINT unset)
// or no store makes it a no-op, not an error.
func TestRestoreTunnelPeersUnconfigured(t *testing.T) {
	st := &fakeStore{instances: []config.Instance{{Name: "node-a", TunnelIP: addr("10.100.0.2")}}}
	if err := NewService(nil, st, nil, nil, nil, &fakeCredentialStore{}).RestoreTunnelPeers(context.Background()); err != nil {
		t.Errorf("RestoreTunnelPeers with nil tunnel: %v", err)
	}

	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{operatorPeer(t)}}
	if err := NewService(nil, nil, nil, nil, tunnels, nil).RestoreTunnelPeers(context.Background()); err != nil {
		t.Errorf("RestoreTunnelPeers with nil store: %v", err)
	}
	if len(tunnels.upsertCall) != 0 {
		t.Errorf("len(upsertCall) = %d with nil store, want 0", len(tunnels.upsertCall))
	}
}

// TestRestoreTunnelPeersReadError: a store that can't list its instances is
// reported, and nothing is upserted.
func TestRestoreTunnelPeersReadError(t *testing.T) {
	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{operatorPeer(t)}}
	st := &fakeStore{instErr: errors.New("database is locked")}

	err := NewService(nil, st, nil, nil, tunnels, &fakeCredentialStore{}).RestoreTunnelPeers(context.Background())
	if err == nil || !errors.Is(err, st.instErr) {
		t.Errorf("RestoreTunnelPeers err = %v, want it to wrap %v", err, st.instErr)
	}
	if len(tunnels.upsertCall) != 0 {
		t.Errorf("len(upsertCall) = %d, want 0", len(tunnels.upsertCall))
	}
}

// TestRestoreTunnelPeersOperatorError: an operator peer that can't be
// registered is returned, since cmd/web no longer registers operators
// anywhere else at startup.
func TestRestoreTunnelPeersOperatorError(t *testing.T) {
	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{operatorPeer(t)}, upsertErr: errors.New("device busy")}

	if err := NewService(nil, &fakeStore{}, nil, nil, tunnels, &fakeCredentialStore{}).RestoreTunnelPeers(context.Background()); err == nil {
		t.Error("RestoreTunnelPeers with a failing operator upsert: err = nil, want an error")
	}
}

// TestRestoreTunnelPeersNilCredentials: with no credential store, NewService's
// documented nil, no instance has a key to register, so only the operators
// are, and startup doesn't panic.
func TestRestoreTunnelPeersNilCredentials(t *testing.T) {
	op := operatorPeer(t)
	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{op}}
	st := &fakeStore{instances: []config.Instance{{Name: "node-a", TunnelIP: addr("10.100.0.2")}}}

	if err := NewService(nil, st, nil, nil, tunnels, nil).RestoreTunnelPeers(context.Background()); err != nil {
		t.Fatalf("RestoreTunnelPeers: %v", err)
	}

	if len(tunnels.upsertCall) != 1 || tunnels.upsertCall[0].pub != op.PublicKey {
		t.Errorf("upsertCall = %+v, want only the operator", tunnels.upsertCall)
	}
}
