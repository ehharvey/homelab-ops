package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"filippo.io/age"
)

// The store's key routes (#197, docs/Decisions.md §28). Each answers with
// the lock state, {"state": "no_store"|"locked"|"unlocked", "store":
// "file"|"memory"}; reset adds "moved_aside", where the old store file went.
// A key goes only in a request body, never in a URL.

// keyBody is the key routes' request body (internal/server's keyRequest).
type keyBody struct {
	Key     string `json:"key,omitempty"`
	NewKey  string `json:"new_key,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// LockState is GET /store/state: whether the store is initialized and
// unlocked. It is answered while locked.
func (c *Client) LockState(ctx context.Context) ([]byte, error) {
	return c.doJSON(ctx, http.MethodGet, "/store/state")
}

// Init is POST /store/init: start an empty store under key. Only a web app
// with no store accepts it.
func (c *Client) Init(ctx context.Context, key *age.X25519Identity) ([]byte, error) {
	return c.keyRoute(ctx, "/store/init", keyBody{Key: key.String()})
}

// Unlock is POST /store/unlock: supply the store's key after a restart. A
// key that doesn't decrypt the store is a 403, and nothing is written.
func (c *Client) Unlock(ctx context.Context, key *age.X25519Identity) ([]byte, error) {
	return c.keyRoute(ctx, "/store/unlock", keyBody{Key: key.String()})
}

// Rotate is POST /store/rotate: re-encrypt the unlocked store from old, the
// key it holds, to next.
func (c *Client) Rotate(ctx context.Context, old, next *age.X25519Identity) ([]byte, error) {
	return c.keyRoute(ctx, "/store/rotate", keyBody{Key: old.String(), NewKey: next.String()})
}

// Reset is POST /store/reset, for a lost key: the locked web app moves its
// store file aside and starts empty under next. The caller must have had
// the operator confirm it; this sends the confirmation.
func (c *Client) Reset(ctx context.Context, next *age.X25519Identity) ([]byte, error) {
	return c.keyRoute(ctx, "/store/reset", keyBody{NewKey: next.String(), Confirm: true})
}

func (c *Client) keyRoute(ctx context.Context, path string, b keyBody) ([]byte, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, NotSent(fmt.Errorf("POST %s: encode request: %w", path, err))
	}
	return c.sendJSON(ctx, http.MethodPost, path, body)
}
