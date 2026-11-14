package store

import (
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// SnapshotDigest moves with the fleet and the commit, but not with the sync
// time or the credentials, which Replace doesn't write.
func TestSnapshotDigestIgnoresOnlyTheSyncTime(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	digest := func() [32]byte {
		t.Helper()
		d, err := s.SnapshotDigest(ctx)
		if err != nil {
			t.Fatalf("SnapshotDigest: %v", err)
		}
		return d
	}
	empty := digest()
	if err := s.Replace(ctx, sampleConfig(), "abc123", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	synced := digest()
	if synced == empty {
		t.Fatal("the digest didn't change with the first sync")
	}

	if err := s.Replace(ctx, sampleConfig(), "abc123", now.Add(time.Hour)); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if digest() != synced {
		t.Error("the digest changed with only the sync time")
	}
	if err := s.SetInstanceCredential(ctx, sampleConfig().Instances[0].Name, nodeprovision.Credential{WireGuardPrivateKey: wireguard.PrivateKey{1}}); err != nil {
		t.Fatalf("SetInstanceCredential: %v", err)
	}
	if digest() != synced {
		t.Error("the digest changed with a credential")
	}

	if err := s.Replace(ctx, sampleConfig(), "def456", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	recommitted := digest()
	if recommitted == synced {
		t.Error("the digest didn't change with the commit")
	}
	changed := sampleConfig()
	changed.Instances[0].Disk += "x"
	if err := s.Replace(ctx, changed, "def456", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if digest() == recommitted {
		t.Error("the digest didn't change with an instance")
	}
}
