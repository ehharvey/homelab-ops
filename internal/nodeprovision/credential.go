// Package nodeprovision provides the mechanism the web app uses to
// authenticate directly against a node's Incus API over its WireGuard
// tunnel and perform a one-time instance-creation call — the exact
// mechanism #92's `bootstrap deploy-agent` will use for real to deploy the
// app-manager agent; #91 only proves the mechanism against a throwaway
// placeholder. It never uses the deployment's break-glass cert
// (internal/seed's clientCertPEM), since the web app doesn't hold that
// cert's private key (docs/Decisions.md §4) — instead it mints its own
// narrow, one-shot credential per instance and revokes it immediately
// after use.
package nodeprovision

import (
	"context"
	"fmt"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// Credential is one instance's persisted secret identity: a WireGuard
// keypair (its private half embedded into that instance's own
// network.yaml at seed-render time) and a bootstrap Incus client cert/key
// pair (its public half preseeded into that instance's incus.yaml as an
// additional trusted client cert, alongside the break-glass one). Minted
// once per instance name and never regenerated — see EnsureCredential.
type Credential struct {
	WireGuardPrivateKey wireguard.PrivateKey
	BootstrapCertPEM    []byte
	BootstrapKeyPEM     []byte
}

// CredentialStore persists Credentials, keyed by instance name,
// independent of the synced networks/instances snapshot —
// internal/store.Store.Replace's full delete+reinsert of the instances
// table never touches this data. Implemented by internal/store.Store.
//
// Deliberately kept separate from config.Instance and Store's
// Networks/Instances listing methods, which internal/server's
// unauthenticated GET /instances route serves directly — see
// docs/Decisions.md's security note on this issue: secret material must
// never become reachable through that generic listing path.
//
// SetInstanceCredential must insert at most once per name and silently
// discard a second call for a name that already has a persisted
// credential (internal/store's implementation does this via
// ON CONFLICT DO NOTHING, not a replace) — EnsureCredential's safety
// under concurrent calls for the same name depends on this.
type CredentialStore interface {
	InstanceCredential(ctx context.Context, name string) (Credential, bool, error)
	SetInstanceCredential(ctx context.Context, name string, cred Credential) error
}

// bootstrapCertValidityDays is short: this cert is used once, immediately
// after its instance's node boots, then revoked — it is not meant to be a
// standing credential the way the break-glass cert is. Since
// EnsureCredential never regenerates once minted, a node imaged from a
// credential that then sits unflashed for longer than this would fail its
// one-time bootstrap Incus auth on first real use — accepted for now
// given how far out that edge case is; revisit if operators start
// pre-building images well ahead of flashing them.
const bootstrapCertValidityDays = 30

// EnsureCredential returns name's persisted Credential, minting a fresh
// WireGuard keypair and a short-lived Incus client cert on first call for
// that name and persisting the result. Never regenerates on later calls:
// a changed keypair would silently invalidate whatever's already been
// baked into a possibly-already-flashed image.
//
// Safe under concurrent calls for the same name — this function is called
// from two paths with no shared lock between them (a manual
// POST /instances/{name}/seed can race the background sync poller's
// reconcile pass for a brand-new instance). The check-then-act here isn't
// atomic on its own, so SetInstanceCredential's underlying insert is
// (INSERT ... ON CONFLICT DO NOTHING — see internal/store): whichever
// concurrent caller's insert lands first wins, the other's is silently
// discarded rather than overwriting it, and every caller re-reads
// afterward to return the one credential that actually persisted. Without
// this, two callers could each mint a *different* keypair/cert for the
// same never-before-seen instance and both write via a last-write-wins
// replace, leaving whichever one already got returned to its caller (and
// possibly already baked into a flashed image) silently mismatched with
// what the store — and therefore reconcileTunnelPeers's UpsertPeer, and
// any future CreateInstance call — actually holds.
func EnsureCredential(ctx context.Context, store CredentialStore, name string) (Credential, error) {
	if cred, ok, err := store.InstanceCredential(ctx, name); err != nil {
		return Credential{}, fmt.Errorf("read credential for %q: %w", name, err)
	} else if ok {
		return cred, nil
	}

	wgPriv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		return Credential{}, fmt.Errorf("generate wireguard keypair for %q: %w", name, err)
	}

	pair, err := cert.Generate(cert.Options{
		CommonName:   "bootstrap@" + name,
		ValidityDays: bootstrapCertValidityDays,
	})
	if err != nil {
		return Credential{}, fmt.Errorf("generate bootstrap cert for %q: %w", name, err)
	}

	cred := Credential{
		WireGuardPrivateKey: wgPriv,
		BootstrapCertPEM:    pair.CertPEM,
		BootstrapKeyPEM:     pair.KeyPEM,
	}
	if err := store.SetInstanceCredential(ctx, name, cred); err != nil {
		return Credential{}, fmt.Errorf("persist credential for %q: %w", name, err)
	}

	// SetInstanceCredential only ever inserts once per name — if a
	// concurrent caller already won this race, cred above was just
	// discarded in favor of theirs. Re-read to return whichever credential
	// actually ended up persisted, so every caller (winner or loser)
	// returns the same, single answer.
	stored, ok, err := store.InstanceCredential(ctx, name)
	if err != nil {
		return Credential{}, fmt.Errorf("read credential for %q after insert: %w", name, err)
	}
	if !ok {
		return Credential{}, fmt.Errorf("credential for %q missing immediately after insert", name)
	}
	return stored, nil
}
