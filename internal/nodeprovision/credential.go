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
type CredentialStore interface {
	InstanceCredential(ctx context.Context, name string) (Credential, bool, error)
	SetInstanceCredential(ctx context.Context, name string, cred Credential) error
}

// bootstrapCertValidityDays is short: this cert is used once, immediately
// after its instance's node boots, then revoked — it is not meant to be a
// standing credential the way the break-glass cert is.
const bootstrapCertValidityDays = 30

// EnsureCredential returns name's persisted Credential, minting a fresh
// WireGuard keypair and a short-lived Incus client cert on first call for
// that name and persisting the result. Never regenerates on later calls:
// a changed keypair would silently invalidate whatever's already been
// baked into a possibly-already-flashed image.
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
	return cred, nil
}
