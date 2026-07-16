package nodeprovision

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/cert"
)

// fakeIncusServer is a minimal stand-in for a node's Incus API, just enough
// of the REST surface CreateInstance drives: POST /1.0/instances (async
// create), GET /1.0/operations/{id}/wait, and DELETE
// /1.0/certificates/{fingerprint}. Runs over TLS since CreateInstance
// always dials "https://".
type fakeIncusServer struct {
	srv *httptest.Server

	waitStatusCode lxcapi.StatusCode // defaults to Success if zero... see newFakeIncusServer
	createErr      bool

	revokeCalled        atomic.Bool
	mu                  sync.Mutex
	revokedFingerprints []string
}

func newFakeIncusServer(t *testing.T) *fakeIncusServer {
	t.Helper()
	f := &fakeIncusServer{waitStatusCode: lxcapi.Success}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		if f.createErr {
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Error: "synthetic create failure"})
			return
		}
		op := lxcapi.Operation{ID: "OP1", Status: "Running", StatusCode: lxcapi.Running}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.AsyncResponse, Metadata: meta})
	})
	mux.HandleFunc("GET /1.0/operations/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		op := lxcapi.Operation{ID: r.PathValue("id"), StatusCode: f.waitStatusCode}
		if f.waitStatusCode == lxcapi.Failure {
			op.Err = "synthetic operation failure"
		}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})
	mux.HandleFunc("DELETE /1.0/certificates/{fingerprint}", func(w http.ResponseWriter, r *http.Request) {
		f.revokeCalled.Store(true)
		f.mu.Lock()
		f.revokedFingerprints = append(f.revokedFingerprints, r.PathValue("fingerprint"))
		f.mu.Unlock()
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse})
	})

	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeEnvelope(w http.ResponseWriter, resp lxcapi.Response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// addr returns the server's host:port, e.g. "127.0.0.1:54321".
func (f *fakeIncusServer) addr() string {
	return strings.TrimPrefix(f.srv.URL, "https://")
}

// dial is a DialFunc that connects to this fake server over a plain TCP
// dial — standing in for (*wireguard.Tunnel).DialContext in these tests,
// which only exercise the HTTP-level create/wait/revoke logic, not the
// WireGuard transport itself.
func (f *fakeIncusServer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

func sampleBootstrapCredential(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	pair, err := cert.Generate(cert.Options{CommonName: "bootstrap@node0", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	return pair.CertPEM, pair.KeyPEM
}

func TestCreateInstanceSuccess(t *testing.T) {
	f := newFakeIncusServer(t)
	certPEM, keyPEM := sampleBootstrapCredential(t)

	err := CreateInstance(context.Background(), f.dial, f.addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if !f.revokeCalled.Load() {
		t.Error("CreateInstance did not revoke the bootstrap cert after a successful create")
	}

	wantFingerprint, err := cert.Fingerprint(certPEM)
	if err != nil {
		t.Fatalf("cert.Fingerprint: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.revokedFingerprints) != 1 || f.revokedFingerprints[0] != wantFingerprint {
		t.Errorf("revoked fingerprints = %v, want [%s]", f.revokedFingerprints, wantFingerprint)
	}
}

func TestCreateInstanceCreateFailure(t *testing.T) {
	f := newFakeIncusServer(t)
	f.createErr = true
	certPEM, keyPEM := sampleBootstrapCredential(t)

	err := CreateInstance(context.Background(), f.dial, f.addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err == nil {
		t.Fatal("CreateInstance with a failing create call = nil error, want non-nil")
	}
	// The cert must still be revoked even though creation failed — it's a
	// deferred call, not conditional on success.
	if !f.revokeCalled.Load() {
		t.Error("CreateInstance did not revoke the bootstrap cert after a failed create")
	}
}

func TestCreateInstanceOperationFailure(t *testing.T) {
	f := newFakeIncusServer(t)
	f.waitStatusCode = lxcapi.Failure
	certPEM, keyPEM := sampleBootstrapCredential(t)

	err := CreateInstance(context.Background(), f.dial, f.addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err == nil {
		t.Fatal("CreateInstance with a failed operation = nil error, want non-nil")
	}
	if !f.revokeCalled.Load() {
		t.Error("CreateInstance did not revoke the bootstrap cert after an operation failure")
	}
}

func TestCreateInstanceRevokeFailureDoesNotMaskSuccess(t *testing.T) {
	certPEM, keyPEM := sampleBootstrapCredential(t)

	// A bespoke server (rather than newFakeIncusServer) whose create/wait
	// path succeeds normally but whose DELETE always 500s, so the revoke
	// step specifically fails while the create itself does not.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		op := lxcapi.Operation{ID: "OP1", StatusCode: lxcapi.Running}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.AsyncResponse, Metadata: meta})
	})
	mux.HandleFunc("GET /1.0/operations/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		op := lxcapi.Operation{ID: r.PathValue("id"), StatusCode: lxcapi.Success}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})
	mux.HandleFunc("DELETE /1.0/certificates/{fingerprint}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, "synthetic server error")
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}

	err := CreateInstance(context.Background(), dial, strings.TrimPrefix(srv.URL, "https://"), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateInstance = %v, want nil (a revoke failure must not mask a successful create)", err)
	}
}
