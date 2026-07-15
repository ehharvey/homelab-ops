package wireguard

import (
	"context"
	"fmt"
)

// IdentityStore persists the web app's own WireGuard private key across
// restarts. Implemented by internal/store.Store.
type IdentityStore interface {
	WireGuardPrivateKey(ctx context.Context) (key PrivateKey, ok bool, err error)
	SetWireGuardPrivateKey(ctx context.Context, key PrivateKey) error
}

// LoadOrGenerateIdentity returns the web app's persisted private key,
// generating and persisting a fresh one on first call. The app's identity
// is app-generated (unlike the operator-supplied break-glass cert), so it
// belongs in the durable store rather than a new operator-facing file path
// — no new deployment config surface for the operator to provide.
func LoadOrGenerateIdentity(ctx context.Context, store IdentityStore) (PrivateKey, error) {
	priv, ok, err := store.WireGuardPrivateKey(ctx)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("read wireguard identity: %w", err)
	}
	if ok {
		return priv, nil
	}

	priv, _, err = GenerateKeypair()
	if err != nil {
		return PrivateKey{}, fmt.Errorf("generate wireguard identity: %w", err)
	}
	if err := store.SetWireGuardPrivateKey(ctx, priv); err != nil {
		return PrivateKey{}, fmt.Errorf("persist wireguard identity: %w", err)
	}
	return priv, nil
}
