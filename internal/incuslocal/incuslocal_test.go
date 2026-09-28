package incuslocal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// fakeIncus is a minimal stand-in for an Incus daemon, served over a real
// unix socket rather than TCP — the socket is the whole point of this
// package, so stubbing it out would leave Dial's transport untested.
//
// Mirrors internal/nodeprovision's fakeIncusServer, which uses
// httptest.NewTLSServer for the same purpose on its own transport.
type fakeIncus struct {
	srv    *httptest.Server
	socket string

	mu sync.Mutex
	// instances is the listing GET /1.0/instances?recursion=1 returns.
	instances []lxcapi.Instance
	// patched records the config maps SetUserKeys sent, per instance.
	patched map[string]map[string]string
	// deleted records the names DeleteInstance asked to remove.
	deleted []string
	// created records the requests CreateInstance sent.
	created []lxcapi.InstancesPost
	// files is the content ReadFile serves, keyed "<instance>:<path>".
	files map[string]string
	// dirs marks paths that should answer as directories.
	dirs map[string]bool
	// patchAsync makes PATCH answer with an operation instead of a sync
	// response, the shape a running instance can produce.
	patchAsync bool
	// operationFails makes any awaited operation finish as a failure.
	operationFails bool
}

func newFakeIncus(t *testing.T) *fakeIncus {
	t.Helper()
	f := &fakeIncus{
		patched: map[string]map[string]string{},
		files:   map[string]string{},
		dirs:    map[string]bool{},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		meta, _ := json.Marshal(f.instances)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})

	mux.HandleFunc("POST /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		var req lxcapi.InstancesPost
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Error: "undecodable body"})
			return
		}
		f.mu.Lock()
		f.created = append(f.created, req)
		f.mu.Unlock()
		writeOperation(w)
	})

	mux.HandleFunc("DELETE /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("name"))
		f.mu.Unlock()
		writeOperation(w)
	})

	mux.HandleFunc("PATCH /1.0/instances/{name}", func(w http.ResponseWriter, r *http.Request) {
		var put lxcapi.InstancePut
		if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Error: "undecodable body"})
			return
		}
		f.mu.Lock()
		name := r.PathValue("name")
		if f.patched[name] == nil {
			f.patched[name] = map[string]string{}
		}
		for k, v := range put.Config {
			f.patched[name][k] = v
		}
		async := f.patchAsync
		f.mu.Unlock()

		if async {
			writeOperation(w)
			return
		}
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse})
	})

	mux.HandleFunc("GET /1.0/instances/{name}/files", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("name") + ":" + r.URL.Query().Get("path")
		f.mu.Lock()
		content, ok := f.files[key]
		isDir := f.dirs[key]
		f.mu.Unlock()

		if isDir {
			w.Header().Set("X-Incus-type", "directory")
			meta, _ := json.Marshal([]string{"a", "b"})
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Code: http.StatusNotFound, Error: "not found"})
			return
		}
		w.Header().Set("X-Incus-type", "file")
		_, _ = w.Write([]byte(content))
	})

	mux.HandleFunc("GET /1.0/operations/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fails := f.operationFails
		f.mu.Unlock()

		op := lxcapi.Operation{ID: r.PathValue("id"), StatusCode: lxcapi.Success}
		if fails {
			op.StatusCode = lxcapi.Failure
			op.Err = "synthetic operation failure"
		}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})

	// A unix socket path has a hard length limit around 108 bytes, and
	// t.TempDir() embeds the test name, so a long one can overflow it. Keep
	// the basename minimal and fail loudly rather than mysteriously.
	f.socket = filepath.Join(t.TempDir(), "s.sock")
	if len(f.socket) > 100 {
		t.Fatalf("socket path too long for a unix socket (%d bytes): %s", len(f.socket), f.socket)
	}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}

	f.srv = httptest.NewUnstartedServer(mux)
	f.srv.Listener.Close() //nolint:errcheck,gosec // replaced by the unix listener below
	f.srv.Listener = ln
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func writeEnvelope(w http.ResponseWriter, resp lxcapi.Response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func writeOperation(w http.ResponseWriter) {
	op := lxcapi.Operation{ID: "OP1", Status: "Running", StatusCode: lxcapi.Running}
	meta, _ := json.Marshal(op)
	writeEnvelope(w, lxcapi.Response{Type: lxcapi.AsyncResponse, Metadata: meta})
}

func (f *fakeIncus) client(t *testing.T) *Client {
	t.Helper()
	c, err := Dial(f.socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

func managed(name, app, status string) lxcapi.Instance {
	return lxcapi.Instance{
		Name:   name,
		Status: status,
		InstancePut: lxcapi.InstancePut{Config: lxcapi.ConfigMap{
			AppTagKey: app,
		}},
	}
}

func TestDialRejectsMissingAndNonSocketPaths(t *testing.T) {
	if _, err := Dial(""); err == nil {
		t.Fatal("Dial(\"\") succeeded, want an error")
	}
	if _, err := Dial(filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Fatal("Dial on a missing path succeeded, want an error")
	}

	// A regular file is the mistake worth catching: it exists, so a bare
	// existence check would pass it and fail confusingly at first use.
	regular := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := Dial(regular)
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Dial on a regular file: got %v, want a 'not a socket' error", err)
	}
}

func TestListInstancesReturnsOnlyManagedInstancesWithStatus(t *testing.T) {
	f := newFakeIncus(t)

	unmanaged := lxcapi.Instance{
		Name:        "someones-database",
		Status:      "Running",
		InstancePut: lxcapi.InstancePut{Config: lxcapi.ConfigMap{"limits.cpu": "4"}},
	}
	stopped := managed("agent-node0-g2", "agent", "Stopped")
	stopped.Location = "none" // what an unclustered Incus actually reports
	running := managed("agent-node0-g3", "agent", "Running")
	running.Location = "node0"
	running.Config["limits.cpu"] = "2"

	f.instances = []lxcapi.Instance{unmanaged, stopped, running}

	got, err := f.client(t).ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d instances (%+v), want 2 — the untagged one must be excluded", len(got), got)
	}

	byName := map[string]Instance{}
	for _, inst := range got {
		byName[inst.Name] = inst
	}
	if _, ok := byName["someones-database"]; ok {
		t.Error("an instance without " + AppTagKey + " was returned; nothing unmanaged may be")
	}

	// Status must survive the projection: leaderelection.Registry.Peers has
	// to drop non-running instances, and cannot if it never sees status.
	if s := byName["agent-node0-g2"].Status; s != "Stopped" {
		t.Errorf("stopped instance status = %q, want %q", s, "Stopped")
	}
	if s := byName["agent-node0-g3"].Status; s != "Running" {
		t.Errorf("running instance status = %q, want %q", s, "Running")
	}
	if loc := byName["agent-node0-g3"].Location; loc != "node0" {
		t.Errorf("Location = %q, want %q", loc, "node0")
	}
	// Incus reports "none" rather than "" on an unclustered daemon (verified
	// against a real one). Passing it through unchanged is what lets a caller
	// distinguish the two; translating it to "" here would invite
	// `Location == ""` checks that never fire on a clustered host.
	if loc := byName["agent-node0-g2"].Location; loc != "none" {
		t.Errorf("unclustered Location = %q, want the literal %q passed through", loc, "none")
	}
	// Config is deliberately the whole map, not narrowed to user.*.
	if cpu := byName["agent-node0-g3"].Config["limits.cpu"]; cpu != "2" {
		t.Errorf("Config[limits.cpu] = %q, want %q — Config must not be narrowed", cpu, "2")
	}
}

func TestListInstancesSurfacesIncusError(t *testing.T) {
	f := newFakeIncus(t)
	// Replace the listing handler's data with an error envelope by pointing
	// the client at a server whose mux has no such route registered.
	f.srv.Config.Handler = http.NewServeMux()

	if _, err := f.client(t).ListInstances(context.Background()); err == nil {
		t.Fatal("ListInstances against a server with no route succeeded, want an error")
	}
}

func TestCreateInstanceWaitsForTheOperation(t *testing.T) {
	f := newFakeIncus(t)
	c := f.client(t)

	req := lxcapi.InstancesPost{Name: "agent-node0-g1", Type: lxcapi.InstanceTypeContainer}
	if err := c.CreateInstance(context.Background(), req); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(f.created) != 1 || f.created[0].Name != "agent-node0-g1" {
		t.Fatalf("created = %+v, want one request for agent-node0-g1", f.created)
	}

	// "Operation created" is not success — #161 was exactly a create that
	// returned an operation and then failed. A failed operation must be an
	// error from CreateInstance, not a silent pass.
	f.operationFails = true
	err := c.CreateInstance(context.Background(), lxcapi.InstancesPost{Name: "doomed"})
	if err == nil {
		t.Fatal("CreateInstance succeeded despite a failed operation")
	}
	if !strings.Contains(err.Error(), "synthetic operation failure") {
		t.Errorf("error %q does not carry the operation's own message", err)
	}
}

func TestCreateInstanceRequiresAName(t *testing.T) {
	f := newFakeIncus(t)
	if err := f.client(t).CreateInstance(context.Background(), lxcapi.InstancesPost{}); err == nil {
		t.Fatal("CreateInstance with no name succeeded, want an error")
	}
}

func TestDeleteInstance(t *testing.T) {
	f := newFakeIncus(t)
	c := f.client(t)

	if err := c.DeleteInstance(context.Background(), "agent-node0-g2"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "agent-node0-g2" {
		t.Fatalf("deleted = %v, want [agent-node0-g2]", f.deleted)
	}

	if err := c.DeleteInstance(context.Background(), ""); err == nil {
		t.Fatal("DeleteInstance with no name succeeded, want an error")
	}

	f.operationFails = true
	if err := c.DeleteInstance(context.Background(), "agent-node0-g2"); err == nil {
		t.Fatal("DeleteInstance succeeded despite a failed operation")
	}
}

func TestSetUserKeysHandlesSyncAndAsyncResponses(t *testing.T) {
	// Which shape Incus sends is not predictable from the request: the #160
	// spike measured a synchronous 200 for a config-only PATCH on a
	// never-started instance, but a running one can hand back an operation.
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", async), func(t *testing.T) {
			f := newFakeIncus(t)
			f.patchAsync = async

			keys := map[string]string{
				"user.homelab-ops.epoch":         "7",
				"user.homelab-ops.healthy-since": "2026-09-28T00:00:00Z",
			}
			if err := f.client(t).SetUserKeys(context.Background(), "agent-node0-g3", keys); err != nil {
				t.Fatalf("SetUserKeys: %v", err)
			}
			got := f.patched["agent-node0-g3"]
			for k, want := range keys {
				if got[k] != want {
					t.Errorf("patched[%s] = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestSetUserKeysRefusesNonUserKeys(t *testing.T) {
	f := newFakeIncus(t)
	c := f.client(t)

	// The guardrail keeps a narrow metadata writer from becoming a general
	// config writer able to change limits or devices.
	err := c.SetUserKeys(context.Background(), "agent-node0-g3", map[string]string{
		"user.homelab-ops.epoch": "7",
		"limits.cpu":             "64",
	})
	if err == nil {
		t.Fatal("SetUserKeys accepted a non-user key, want a refusal")
	}
	if !strings.Contains(err.Error(), "limits.cpu") {
		t.Errorf("error %q does not name the offending key", err)
	}
	// Nothing may have been sent: the check runs before the request.
	if len(f.patched) != 0 {
		t.Errorf("patched = %+v, want nothing sent when a key is rejected", f.patched)
	}
}

func TestSetUserKeysRequiresNameAndKeys(t *testing.T) {
	f := newFakeIncus(t)
	c := f.client(t)

	if err := c.SetUserKeys(context.Background(), "", map[string]string{"user.x": "1"}); err == nil {
		t.Error("SetUserKeys with no instance name succeeded, want an error")
	}
	if err := c.SetUserKeys(context.Background(), "agent-node0-g3", nil); err == nil {
		t.Error("SetUserKeys with no keys succeeded, want an error")
	}
}

func TestReadFile(t *testing.T) {
	f := newFakeIncus(t)
	f.files["agent-node0-g3:/run/agent/heartbeat"] = "2026-09-28T01:02:03Z\n"
	c := f.client(t)

	got, err := c.ReadFile(context.Background(), "agent-node0-g3", "/run/agent/heartbeat")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "2026-09-28T01:02:03Z\n" {
		t.Errorf("ReadFile = %q, want the heartbeat content", got)
	}
}

func TestReadFileMissingFileIsAnErrorNotEmptyContent(t *testing.T) {
	f := newFakeIncus(t)
	c := f.client(t)

	// The property that matters: a heartbeat check reading "absent" as
	// "fine" would invert the thing it tests.
	got, err := c.ReadFile(context.Background(), "agent-node0-g3", "/run/agent/heartbeat")
	if err == nil {
		t.Fatalf("ReadFile on a missing file returned %q and no error", got)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not carry Incus's own message", err)
	}
	if got != nil {
		t.Errorf("ReadFile returned %q alongside an error, want nil", got)
	}
}

func TestReadFileRejectsADirectory(t *testing.T) {
	f := newFakeIncus(t)
	f.dirs["agent-node0-g3:/run/agent"] = true

	_, err := f.client(t).ReadFile(context.Background(), "agent-node0-g3", "/run/agent")
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("ReadFile on a directory: got %v, want an 'is a directory' error", err)
	}
}

func TestReadFileRequiresNameAndPath(t *testing.T) {
	f := newFakeIncus(t)
	c := f.client(t)

	if _, err := c.ReadFile(context.Background(), "", "/x"); err == nil {
		t.Error("ReadFile with no instance name succeeded, want an error")
	}
	if _, err := c.ReadFile(context.Background(), "agent-node0-g3", ""); err == nil {
		t.Error("ReadFile with no path succeeded, want an error")
	}
}

func TestReadFileRejectsOversizedContent(t *testing.T) {
	f := newFakeIncus(t)
	f.files["agent-node0-g3:/big"] = strings.Repeat("x", maxFileBytes+1)

	_, err := f.client(t).ReadFile(context.Background(), "agent-node0-g3", "/big")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("ReadFile on an oversized file: got %v, want a size-limit error", err)
	}
}

func TestReadFileAtExactlyTheLimitSucceeds(t *testing.T) {
	// The cap reads one byte past the limit precisely so that a file of
	// exactly maxFileBytes is not mistaken for an oversized one.
	f := newFakeIncus(t)
	f.files["agent-node0-g3:/exact"] = strings.Repeat("x", maxFileBytes)

	got, err := f.client(t).ReadFile(context.Background(), "agent-node0-g3", "/exact")
	if err != nil {
		t.Fatalf("ReadFile at exactly the limit: %v", err)
	}
	if len(got) != maxFileBytes {
		t.Errorf("read %d bytes, want %d", len(got), maxFileBytes)
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	f := newFakeIncus(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.client(t).ListInstances(ctx)
	if err == nil {
		t.Fatal("ListInstances with a cancelled context succeeded, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}
