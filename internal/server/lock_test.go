package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/sealed"
	"github.com/ehharvey/homelab-ops/internal/vault"
)

// apiRoutes is every route NewFromService serves besides /healthz: a locked
// web app must refuse each one.
var apiRoutes = []struct{ method, path string }{
	{http.MethodPost, "/sync"},
	{http.MethodGet, "/status"},
	{http.MethodGet, "/networks"},
	{http.MethodGet, "/instances"},
	{http.MethodPost, "/instances/node0/seed"},
	{http.MethodGet, "/instances/node0/image"},
	{http.MethodPost, "/instances/node0/deploy-agent"},
	{http.MethodGet, "/no-such-route"},
}

func storeKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := sealed.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return id
}

// lockedApp is the web app's handler over a vault at path (":memory:" for
// none), as cmd/web composes it, and how many times onUnlock ran.
func lockedApp(t *testing.T, path string) (http.Handler, *int) {
	t.Helper()
	v, err := vault.New(path)
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	syncer := &fakeSyncer{cfg: config.Config{}, sha: "abc"}
	svc := NewService(syncer, v, nil, nil, nil, v)
	var unlocks int
	h := WithLock(v, NewFromService(svc), func(context.Context) error { unlocks++; return nil })
	return h, &unlocks
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func keyJSON(fields map[string]any) string {
	b, _ := json.Marshal(fields)
	return string(b)
}

func wantState(t *testing.T, h http.Handler, want vault.State) {
	t.Helper()
	rec := call(t, h, http.MethodGet, "/store/state", "")
	var got LockState
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.State != want {
		t.Errorf("GET /store/state = %d %q, want 200 with state %s", rec.Code, rec.Body.String(), want)
	}
}

func wantRefused(t *testing.T, h http.Handler, why string) {
	t.Helper()
	for _, r := range apiRoutes {
		rec := call(t, h, r.method, r.path, "")
		if rec.Code != http.StatusLocked {
			t.Errorf("%s: %s %s = %d %q, want 423", why, r.method, r.path, rec.Code, rec.Body.String())
		}
	}
	if rec := call(t, h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("%s: GET /healthz = %d, want 200", why, rec.Code)
	}
}

// After every start, with or without a store file, only /healthz, the lock
// state and the verbs the state accepts are answered.
func TestLockedAppServesOnlyHealthStateAndKeyRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.db")
	h, unlocks := lockedApp(t, path)

	wantState(t, h, vault.NoStore)
	wantRefused(t, h, "no store")
	if rec := call(t, h, http.MethodPost, "/store/unlock", keyJSON(map[string]any{"key": storeKey(t).String()})); rec.Code != http.StatusConflict {
		t.Errorf("unlock with no store = %d, want 409", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, "/store/reset", keyJSON(map[string]any{"new_key": storeKey(t).String(), "confirm": true})); rec.Code != http.StatusConflict {
		t.Errorf("reset with no store = %d, want 409", rec.Code)
	}

	k := storeKey(t)
	rec := call(t, h, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": k.String()}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"unlocked"`) {
		t.Fatalf("init = %d %q, want 200 unlocked", rec.Code, rec.Body.String())
	}
	if *unlocks != 1 {
		t.Errorf("onUnlock ran %d times after init, want 1", *unlocks)
	}
	if rec := call(t, h, http.MethodPost, "/sync", ""); rec.Code != http.StatusOK {
		t.Fatalf("POST /sync after init = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodGet, "/status", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"commit":"abc"`) {
		t.Errorf("GET /status after init = %d %q, want 200 at abc", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": storeKey(t).String()})); rec.Code != http.StatusConflict {
		t.Errorf("a second init = %d, want 409", rec.Code)
	}

	// The restart: locked, and only unlock and reset are accepted.
	r, unlocks := lockedApp(t, path)
	wantState(t, r, vault.Locked)
	wantRefused(t, r, "locked")
	if rec := call(t, r, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": storeKey(t).String()})); rec.Code != http.StatusConflict {
		t.Errorf("init on a locked store = %d, want 409", rec.Code)
	}
	if rec := call(t, r, http.MethodPost, "/store/unlock", keyJSON(map[string]any{"key": storeKey(t).String()})); rec.Code != http.StatusForbidden {
		t.Errorf("unlock with a wrong key = %d %q, want 403", rec.Code, rec.Body.String())
	}
	wantRefused(t, r, "after a wrong key")
	if rec := call(t, r, http.MethodPost, "/store/unlock", keyJSON(map[string]any{"key": k.String()})); rec.Code != http.StatusOK {
		t.Fatalf("unlock with the key = %d %q, want 200", rec.Code, rec.Body.String())
	}
	if *unlocks != 1 {
		t.Errorf("onUnlock ran %d times after unlock, want 1", *unlocks)
	}
	if rec := call(t, r, http.MethodGet, "/status", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"commit":"abc"`) {
		t.Errorf("GET /status after unlock = %d %q, want the prior state, at abc", rec.Code, rec.Body.String())
	}
}

func TestInMemoryAppIsLockedUntilInit(t *testing.T) {
	h, _ := lockedApp(t, ":memory:")
	wantState(t, h, vault.NoStore)
	wantRefused(t, h, "in-memory store before init")
	rec := call(t, h, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": storeKey(t).String()}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"store":"memory"`) {
		t.Fatalf("init = %d %q, want 200 with store memory", rec.Code, rec.Body.String())
	}
	if rec := call(t, h, http.MethodGet, "/status", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /status after init = %d, want 200", rec.Code)
	}
}

func TestRotateRoute(t *testing.T) {
	h, _ := lockedApp(t, filepath.Join(t.TempDir(), "web.db"))
	if rec := call(t, h, http.MethodPost, "/store/rotate", keyJSON(map[string]any{"key": storeKey(t).String(), "new_key": storeKey(t).String()})); rec.Code != http.StatusConflict {
		t.Errorf("rotate with no store = %d, want 409", rec.Code)
	}
	k := storeKey(t)
	call(t, h, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": k.String()}))
	if rec := call(t, h, http.MethodPost, "/store/rotate", keyJSON(map[string]any{"key": storeKey(t).String(), "new_key": storeKey(t).String()})); rec.Code != http.StatusForbidden {
		t.Errorf("rotate from a key that isn't the store's = %d, want 403", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, "/store/rotate", keyJSON(map[string]any{"key": k.String(), "new_key": storeKey(t).String()})); rec.Code != http.StatusOK {
		t.Errorf("rotate = %d %q, want 200", rec.Code, rec.Body.String())
	}
}

func TestResetRouteNeedsConfirmAndALockedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.db")
	h, _ := lockedApp(t, path)
	call(t, h, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": storeKey(t).String()}))
	if rec := call(t, h, http.MethodPost, "/store/reset", keyJSON(map[string]any{"new_key": storeKey(t).String(), "confirm": true})); rec.Code != http.StatusConflict {
		t.Errorf("reset of an unlocked store = %d, want 409", rec.Code)
	}

	r, unlocks := lockedApp(t, path)
	if rec := call(t, r, http.MethodPost, "/store/reset", keyJSON(map[string]any{"new_key": storeKey(t).String()})); rec.Code != http.StatusBadRequest {
		t.Errorf("reset without confirm = %d, want 400", rec.Code)
	}
	wantState(t, r, vault.Locked)
	rec := call(t, r, http.MethodPost, "/store/reset", keyJSON(map[string]any{"new_key": storeKey(t).String(), "confirm": true}))
	var got LockState
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.State != vault.Unlocked || !strings.HasPrefix(got.MovedAside, path+".reset-") {
		t.Fatalf("reset = %d %q, want 200 unlocked with the moved-aside path", rec.Code, rec.Body.String())
	}
	if *unlocks != 1 {
		t.Errorf("onUnlock ran %d times after reset, want 1", *unlocks)
	}
}

func TestKeyRoutesRefuseMalformedRequests(t *testing.T) {
	h, _ := lockedApp(t, ":memory:")
	for _, body := range []string{"", "{", `{"key":"AGE-SECRET-KEY-1JUNK"}`, `{"key":"x","extra":1}`} {
		rec := call(t, h, http.MethodPost, "/store/init", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("init with %q = %d, want 400", body, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "JUNK") {
			t.Errorf("init's refusal echoes the key: %q", rec.Body.String())
		}
	}
	wantState(t, h, vault.NoStore)
}

// onUnlock's error is logged, never turned into a failed unlock: the store
// is already open.
func TestOnUnlockErrorDoesNotFailTheVerb(t *testing.T) {
	v, err := vault.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close() //nolint:errcheck // test cleanup
	h := WithLock(v, http.NotFoundHandler(), func(context.Context) error { return errors.New("peer restore failed") })
	if rec := call(t, h, http.MethodPost, "/store/init", keyJSON(map[string]any{"key": storeKey(t).String()})); rec.Code != http.StatusOK {
		t.Errorf("init with a failing onUnlock = %d, want 200", rec.Code)
	}
}

// onUnlock's context isn't the request's cancellation: an operator's
// dropped connection mid-unlock can't cut the peer restore short (#269).
func TestOnUnlockOutlivesTheRequest(t *testing.T) {
	v, err := vault.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close() //nolint:errcheck // test cleanup
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotErr error
	h := WithLock(v, http.NotFoundHandler(), func(ctx context.Context) error {
		cancel() // the operator's CLI goes away
		gotErr = ctx.Err()
		return nil
	})
	req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/store/init", strings.NewReader(keyJSON(map[string]any{"key": storeKey(t).String()})))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if reqCtx.Err() == nil {
		t.Fatal("onUnlock didn't run")
	}
	if gotErr != nil {
		t.Errorf("onUnlock's context was cancelled with the request: %v", gotErr)
	}
}
