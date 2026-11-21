package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/sealed"
)

// keyServer records the key routes' requests and answers each with a lock
// state.
type keyServer struct {
	paths  []string
	bodies []map[string]any
}

func (k *keyServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.paths = append(k.paths, r.Method+" "+r.URL.Path)
		var body map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
		}
		k.bodies = append(k.bodies, body)
		_, _ = io.WriteString(w, `{"state":"unlocked","store":"file"}`)
	})
}

func resetStoreFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		apiInitKeyFile, apiUnlockKeyFile = "", ""
		apiRotateFlags.keyFile, apiRotateFlags.newKeyFile = "", ""
		apiResetFlags.newKeyFile, apiResetFlags.yes = "", false
		rootCmd.SetIn(nil)
	}
	reset()
	t.Cleanup(reset)
}

// Only the web app's 4xx refusal, or a request that was never sent, says a
// generated key wasn't taken; a lost answer (a transport error once the
// request was sent, a 5xx) says to keep the file, since deleting a key the
// web app did take locks the store for good after its next restart.
func TestKeyVerbErrorKeepsTheFileUnlessRefused(t *testing.T) {
	if err := keyVerbError(nil, "new.key", "hint"); err != nil {
		t.Errorf("keyVerbError(nil) = %v", err)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&apiclient.APIError{Method: "POST", Path: "/store/rotate", StatusCode: http.StatusForbidden}, "wasn't taken; delete it"},
		{&apiclient.APIError{Method: "POST", Path: "/store/rotate", StatusCode: http.StatusInternalServerError}, "keep the file; hint"},
		{errors.New("POST /store/rotate: connection reset by peer"), "keep the file; hint"},
		// Before the request was written (the tunnel didn't come up, the
		// dial failed), the web app can't have the key.
		{apiclient.NotSent(errors.New("endpoint 192.0.2.1:51820: handshake timed out")), "never reached the web app, so the key generated in new.key wasn't taken; delete it"},
	} {
		if got := keyVerbError(c.err, "new.key", "hint"); got == nil || !strings.Contains(got.Error(), c.want) || !errors.Is(got, c.err) {
			t.Errorf("keyVerbError(%v) = %v, want it wrapped, saying %q", c.err, got, c.want)
		}
	}
}

func TestAPIInitGeneratesTheKeyFileAndSendsIt(t *testing.T) {
	ks := &keyServer{}
	fakeAPI(t, ks.handler())
	resetStoreFlags(t)
	path := filepath.Join(t.TempDir(), "store.key")

	if _, _, err := runAPI("api", "init", "--key-file", path); err != nil {
		t.Fatalf("init: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode %04o, want 0600", perm)
	}
	key, err := sealed.ReadKeyFile(path)
	if err != nil {
		t.Fatalf("ReadKeyFile: %v", err)
	}
	if len(ks.paths) != 1 || ks.paths[0] != "POST /store/init" || ks.bodies[0]["key"] != key.String() {
		t.Errorf("init sent %v %v, want POST /store/init with the file's key", ks.paths, ks.bodies)
	}

	// An existing key file is never overwritten, and nothing is sent.
	resetStoreFlags(t)
	if _, _, err := runAPI("api", "init", "--key-file", path); err == nil {
		t.Error("init over an existing key file = nil error, want a refusal")
	}
	if again, _ := sealed.ReadKeyFile(path); again == nil || again.String() != key.String() {
		t.Error("a refused init changed the key file")
	}
	if len(ks.paths) != 1 {
		t.Errorf("a refused init sent %v", ks.paths[1:])
	}
}

func TestAPIUnlockAndRotateSendTheirKeys(t *testing.T) {
	ks := &keyServer{}
	fakeAPI(t, ks.handler())
	resetStoreFlags(t)
	dir := t.TempDir()
	oldPath, newPath := filepath.Join(dir, "old.key"), filepath.Join(dir, "new.key")
	old, err := sealed.WriteNewKeyFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := runAPI("api", "unlock", "--key-file", oldPath); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, _, err := runAPI("api", "rotate", "--key-file", oldPath, "--new-key-file", newPath); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	next, err := sealed.ReadKeyFile(newPath)
	if err != nil {
		t.Fatalf("rotate's new key file: %v", err)
	}
	if len(ks.paths) != 2 || ks.paths[0] != "POST /store/unlock" || ks.bodies[0]["key"] != old.String() {
		t.Fatalf("unlock sent %v %v, want POST /store/unlock with the key", ks.paths, ks.bodies)
	}
	if ks.paths[1] != "POST /store/rotate" || ks.bodies[1]["key"] != old.String() || ks.bodies[1]["new_key"] != next.String() {
		t.Errorf("rotate sent %v %v, want POST /store/rotate with the old and new keys", ks.paths[1], ks.bodies[1])
	}
	if old.String() == next.String() {
		t.Error("rotate's new key is the old one")
	}

	resetStoreFlags(t)
	if _, _, err := runAPI("api", "unlock", "--key-file", filepath.Join(dir, "missing.key")); err == nil {
		t.Error("unlock with a missing key file = nil error")
	}
	if len(ks.paths) != 2 {
		t.Errorf("a failed unlock sent %v", ks.paths[2:])
	}
}

func TestAPIResetAsksForConfirmation(t *testing.T) {
	ks := &keyServer{}
	fakeAPI(t, ks.handler())
	dir := t.TempDir()

	for _, answer := range []string{"n\n", "\n", ""} {
		resetStoreFlags(t)
		path := filepath.Join(dir, "declined.key")
		rootCmd.SetIn(strings.NewReader(answer))
		_, stderr, err := runAPI("api", "reset", "--new-key-file", path)
		if err == nil || !strings.Contains(err.Error(), "not confirmed") {
			t.Errorf("reset answered %q = %v, want not confirmed", answer, err)
		}
		if !strings.Contains(stderr, "[y/N]") {
			t.Errorf("reset didn't prompt: stderr %q", stderr)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("a declined reset (%q) wrote a key file", answer)
		}
	}
	if len(ks.paths) != 0 {
		t.Fatalf("a declined reset sent %v", ks.paths)
	}

	resetStoreFlags(t)
	rootCmd.SetIn(strings.NewReader("y\n"))
	if _, _, err := runAPI("api", "reset", "--new-key-file", filepath.Join(dir, "yes.key")); err != nil {
		t.Fatalf("reset answered y: %v", err)
	}
	resetStoreFlags(t)
	if _, _, err := runAPI("api", "reset", "--yes", "--new-key-file", filepath.Join(dir, "flag.key")); err != nil {
		t.Fatalf("reset --yes: %v", err)
	}
	if len(ks.paths) != 2 {
		t.Fatalf("confirmed resets sent %v, want two", ks.paths)
	}
	for i, p := range ks.paths {
		if p != "POST /store/reset" || ks.bodies[i]["confirm"] != true || ks.bodies[i]["new_key"] == nil {
			t.Errorf("reset sent %s %v, want POST /store/reset confirmed with a new key", p, ks.bodies[i])
		}
	}
}

// --url calls the host listener directly, with no tunnel flags.
func TestAPIURLSkipsTheTunnel(t *testing.T) {
	resetAPIFlags(t)
	t.Cleanup(func() { resetAPIFlags(t) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/store/state" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"state":"no_store","store":"memory"}`)
	}))
	t.Cleanup(srv.Close)

	out, _, err := runAPI("api", "--url", srv.URL, "lock-state")
	if err != nil {
		t.Fatalf("lock-state --url: %v", err)
	}
	if !strings.Contains(out, `"state":"no_store"`) {
		t.Errorf("lock-state printed %q, want the server's lock state", out)
	}
	if _, _, err := connectAPITunnel(context.Background()); err != nil {
		t.Errorf("connect with --url still set: %v", err)
	}
}
