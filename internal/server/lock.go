package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"filippo.io/age"

	"github.com/ehharvey/homelab-ops/internal/sealed"
	"github.com/ehharvey/homelab-ops/internal/vault"
)

// KeyStore is the store's lock (#197, docs/Decisions.md §28): its state and
// the four verbs that change it. Implemented by internal/vault.Vault, whose
// errors (vault.ErrState, vault.ErrWrongKey) the key routes map to status
// codes.
type KeyStore interface {
	State() vault.State
	Persistent() bool
	Init(ctx context.Context, id *age.X25519Identity) error
	Unlock(ctx context.Context, id *age.X25519Identity) error
	Rotate(ctx context.Context, old, next *age.X25519Identity) error
	Reset(ctx context.Context, next *age.X25519Identity) (movedTo string, err error)
}

// The vault is also the credential store cmd/web hands the Service, and it
// batches a reconcile's mints into one write of the store file.
var (
	_ KeyStore          = (*vault.Vault)(nil)
	_ CredentialBatcher = (*vault.Vault)(nil)
)

// LockState is GET /store/state's answer, and every key route's on success.
// Its fields are a stable, machine-readable contract: scripts read them, and
// so will #508's UI (§32).
type LockState struct {
	// State is vault.State: "no_store", "locked" or "unlocked".
	State vault.State `json:"state"`
	// Store is "file" with a store file (STORE_PATH), "memory" without.
	Store string `json:"store"`
	// MovedAside is where reset moved the old store file.
	MovedAside string `json:"moved_aside,omitempty"`
}

// The key routes' request bodies. Keys are age X25519 identities
// (AGE-SECRET-KEY-1…), as `bootstrap api` generates them.
type keyRequest struct {
	Key string `json:"key"`
	// NewKey is rotate's and reset's key to change to.
	NewKey string `json:"new_key"`
	// Confirm must be true for reset: a stray request can't discard a store.
	Confirm bool `json:"confirm"`
}

// maxKeyBody bounds a key route's request body: two keys and a flag.
const maxKeyBody = 4 << 10

// WithLock serves api only while ks is unlocked (#197). Until then it
// answers only:
//
//   - GET /healthz;
//   - GET /store/state, the lock state;
//   - POST /store/init, when there is no store (no store file, or an
//     in-memory store, which every start begins without);
//   - POST /store/unlock and POST /store/reset, when a store file exists.
//
// Everything else is 423 Locked, with the lock state in the message. The key
// routes stay served once unlocked, for POST /store/rotate and so a repeated
// verb gets a clear 409 rather than a 423. onUnlock runs after a successful
// init, unlock or reset, with the store open, before the answer is sent:
// cmd/web re-registers the stored instances' tunnel peers there (#269) and
// starts its poller. Its context carries the request's values but not its
// cancellation, so an operator's dropped connection can't cut it short. Its
// error is logged, not returned, since the store is already open; retrying
// is onUnlock's own.
//
// It wraps every API listener alike, the tunnel's and the host's: the key
// routes are served wherever the API is (operator, 2026-11-05).
func WithLock(ks KeyStore, api http.Handler, onUnlock func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /store/state", func(w http.ResponseWriter, _ *http.Request) {
		writeLockState(w, ks, "")
	})
	unlocked := func(ctx context.Context) {
		if onUnlock == nil {
			return
		}
		if err := onUnlock(context.WithoutCancel(ctx)); err != nil {
			log.Printf("store unlocked, but: %v", err)
		}
	}
	mux.HandleFunc("POST /store/init", keyRoute(ks, "init", func(ctx context.Context, req keyRequest) (string, error) {
		id, err := parseKey("key", req.Key)
		if err != nil {
			return "", err
		}
		if err := ks.Init(ctx, id); err != nil {
			return "", err
		}
		log.Printf("store: initialized under a new key (%s)", storeKind(ks))
		unlocked(ctx)
		return "", nil
	}))
	mux.HandleFunc("POST /store/unlock", keyRoute(ks, "unlock", func(ctx context.Context, req keyRequest) (string, error) {
		id, err := parseKey("key", req.Key)
		if err != nil {
			return "", err
		}
		if err := ks.Unlock(ctx, id); err != nil {
			return "", err
		}
		log.Print("store: unlocked")
		unlocked(ctx)
		return "", nil
	}))
	mux.HandleFunc("POST /store/rotate", keyRoute(ks, "rotate", func(ctx context.Context, req keyRequest) (string, error) {
		old, err := parseKey("key", req.Key)
		if err != nil {
			return "", err
		}
		next, err := parseKey("new_key", req.NewKey)
		if err != nil {
			return "", err
		}
		if err := ks.Rotate(ctx, old, next); err != nil {
			return "", err
		}
		log.Print("store: key rotated")
		return "", nil
	}))
	mux.HandleFunc("POST /store/reset", keyRoute(ks, "reset", func(ctx context.Context, req keyRequest) (string, error) {
		if !req.Confirm {
			return "", fmt.Errorf("%w: reset discards the store's state; send \"confirm\": true", errBadKeyRequest)
		}
		next, err := parseKey("new_key", req.NewKey)
		if err != nil {
			return "", err
		}
		movedTo, err := ks.Reset(ctx, next)
		if err != nil {
			return "", err
		}
		//nolint:gosec // G706: movedTo is derived from STORE_PATH, operator-supplied deployment config, and %q quotes it
		log.Printf("store: WARNING: reset under a new key; the old store file is kept at %q", movedTo)
		unlocked(ctx)
		return movedTo, nil
	}))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch s := ks.State(); s {
		case vault.Unlocked:
			api.ServeHTTP(w, r)
		case vault.NoStore:
			http.Error(w, "store is not initialized (lock state no_store): run `bootstrap api init` first", http.StatusLocked)
		default:
			http.Error(w, fmt.Sprintf("store is locked (lock state %s): run `bootstrap api unlock` with its key", s), http.StatusLocked)
		}
	}))
	return mux
}

// errBadKeyRequest is a key route's malformed request: 400.
var errBadKeyRequest = errors.New("bad request")

// keyRoute decodes a key route's body, runs verb (name, for the log), and answers with the lock
// state, or the error's status: 400 for a malformed request, 409 for a verb
// the lock state doesn't accept, 403 for a wrong key. A wrong key's message
// is the vault's, which never echoes the key.
func keyRoute(ks KeyStore, name string, verb func(ctx context.Context, req keyRequest) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req keyRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKeyBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "bad request: want a JSON body with the key", http.StatusBadRequest)
			return
		}
		movedTo, err := verb(r.Context(), req)
		switch {
		case err == nil:
			writeLockState(w, ks, movedTo)
		case errors.Is(err, errBadKeyRequest):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, vault.ErrState):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, vault.ErrWrongKey):
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			log.Printf("store: %s: %v", name, err)
			http.Error(w, "store failed: "+err.Error(), http.StatusInternalServerError)
		}
	}
}

// parseKey parses one of a request's keys, naming its field on failure.
func parseKey(field, s string) (*age.X25519Identity, error) {
	id, err := sealed.ParseKey(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errBadKeyRequest, field, err)
	}
	return id, nil
}

func storeKind(ks KeyStore) string {
	if ks.Persistent() {
		return "file"
	}
	return "memory"
}

func writeLockState(w http.ResponseWriter, ks KeyStore, movedTo string) {
	writeJSON(w, LockState{State: ks.State(), Store: storeKind(ks), MovedAside: movedTo})
}

// RegisterOperatorPeers registers the operator peers alone on the live
// tunnel. cmd/web calls it at startup, while the store is still locked, so
// the operators can reach the key routes over the tunnel (#197); the stored
// instances follow on unlock (RestoreTunnelPeers). A nil tunnel makes it a
// no-op; an operator peer that won't register is an error.
func (s *Service) RegisterOperatorPeers(ctx context.Context) error {
	if s.tunnels == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, nOps := reconcileTunnelPeers(ctx, s.tunnels, s.creds, nil)
	ops := len(s.tunnels.OperatorPeers())
	log.Printf("wireguard: registered %d/%d operator peers; stored instance peers follow when the store is unlocked", nOps, ops)
	if nOps < ops {
		return fmt.Errorf("registered %d of %d operator peers", nOps, ops)
	}
	return nil
}
