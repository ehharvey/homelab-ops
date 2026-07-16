package store

import (
	"net/netip"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func TestWireGuardPrivateKeyAbsentByDefault(t *testing.T) {
	s, ctx := openTestStore(t)

	_, ok, err := s.WireGuardPrivateKey(ctx)
	if err != nil {
		t.Fatalf("WireGuardPrivateKey: %v", err)
	}
	if ok {
		t.Error("WireGuardPrivateKey on a fresh store = ok:true, want false")
	}
}

func TestSetWireGuardPrivateKeyThenGet(t *testing.T) {
	s, ctx := openTestStore(t)

	priv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	if err := s.SetWireGuardPrivateKey(ctx, priv); err != nil {
		t.Fatalf("SetWireGuardPrivateKey: %v", err)
	}

	got, ok, err := s.WireGuardPrivateKey(ctx)
	if err != nil || !ok {
		t.Fatalf("WireGuardPrivateKey = ok:%v err:%v, want a persisted key", ok, err)
	}
	if got != priv {
		t.Errorf("WireGuardPrivateKey = %s, want %s", got.Base64(), priv.Base64())
	}
}

func TestSetWireGuardPrivateKeyOverwrites(t *testing.T) {
	s, ctx := openTestStore(t)

	first, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	second, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	if err := s.SetWireGuardPrivateKey(ctx, first); err != nil {
		t.Fatalf("first SetWireGuardPrivateKey: %v", err)
	}
	if err := s.SetWireGuardPrivateKey(ctx, second); err != nil {
		t.Fatalf("second SetWireGuardPrivateKey: %v", err)
	}

	got, ok, err := s.WireGuardPrivateKey(ctx)
	if err != nil || !ok {
		t.Fatalf("WireGuardPrivateKey = ok:%v err:%v", ok, err)
	}
	if got != second {
		t.Errorf("WireGuardPrivateKey = %s, want the second key %s", got.Base64(), second.Base64())
	}
}

func TestInstanceCredentialAbsentByDefault(t *testing.T) {
	s, ctx := openTestStore(t)

	_, ok, err := s.InstanceCredential(ctx, "node0")
	if err != nil {
		t.Fatalf("InstanceCredential: %v", err)
	}
	if ok {
		t.Error("InstanceCredential for an unminted instance = ok:true, want false")
	}
}

func TestSetInstanceCredentialThenGet(t *testing.T) {
	s, ctx := openTestStore(t)

	priv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	want := nodeprovision.Credential{
		WireGuardPrivateKey: priv,
		BootstrapCertPEM:    []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
		BootstrapKeyPEM:     []byte("-----BEGIN EC PRIVATE KEY-----\nfake\n-----END EC PRIVATE KEY-----\n"),
	}

	if err := s.SetInstanceCredential(ctx, "node0", want); err != nil {
		t.Fatalf("SetInstanceCredential: %v", err)
	}

	got, ok, err := s.InstanceCredential(ctx, "node0")
	if err != nil || !ok {
		t.Fatalf("InstanceCredential = ok:%v err:%v, want a persisted credential", ok, err)
	}
	if got.WireGuardPrivateKey != want.WireGuardPrivateKey {
		t.Errorf("WireGuardPrivateKey = %s, want %s", got.WireGuardPrivateKey.Base64(), want.WireGuardPrivateKey.Base64())
	}
	if string(got.BootstrapCertPEM) != string(want.BootstrapCertPEM) {
		t.Errorf("BootstrapCertPEM = %q, want %q", got.BootstrapCertPEM, want.BootstrapCertPEM)
	}
	if string(got.BootstrapKeyPEM) != string(want.BootstrapKeyPEM) {
		t.Errorf("BootstrapKeyPEM = %q, want %q", got.BootstrapKeyPEM, want.BootstrapKeyPEM)
	}
}

func TestInstanceCredentialSurvivesReplace(t *testing.T) {
	s, ctx := openTestStore(t)

	priv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	cred := nodeprovision.Credential{WireGuardPrivateKey: priv, BootstrapCertPEM: []byte("cert"), BootstrapKeyPEM: []byte("key")}
	if err := s.SetInstanceCredential(ctx, "devnode0", cred); err != nil {
		t.Fatalf("SetInstanceCredential: %v", err)
	}

	// Replace fully deletes and reinserts the instances table — the
	// credential must live outside that blast radius (see schema.sql's
	// instance_credentials comment).
	if err := s.Replace(ctx, sampleConfig(), "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	got, ok, err := s.InstanceCredential(ctx, "devnode0")
	if err != nil || !ok {
		t.Fatalf("InstanceCredential after Replace = ok:%v err:%v, want it to survive", ok, err)
	}
	if got.WireGuardPrivateKey != priv {
		t.Errorf("WireGuardPrivateKey after Replace = %s, want %s", got.WireGuardPrivateKey.Base64(), priv.Base64())
	}
}

func TestReplaceRoundTripsTunnelIP(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := sampleConfig()
	cfg.Instances[0].TunnelIP = netip.MustParseAddr("10.100.0.5")

	if err := s.Replace(ctx, cfg, "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	got, ok, err := s.Instance(ctx, cfg.Instances[0].Name)
	if err != nil || !ok {
		t.Fatalf("Instance = ok:%v err:%v", ok, err)
	}
	if got.TunnelIP != cfg.Instances[0].TunnelIP {
		t.Errorf("TunnelIP = %s, want %s", got.TunnelIP, cfg.Instances[0].TunnelIP)
	}
}
