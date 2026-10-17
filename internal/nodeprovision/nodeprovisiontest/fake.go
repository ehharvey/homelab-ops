// Package nodeprovisiontest provides a fake node Incus API for tests of
// internal/nodeprovision and its callers (the web app's deploy-agent route,
// `bootstrap deploy-agent`): just the REST surface nodeprovision drives,
// over TLS, with the revoke-then-reject behaviour of a real node.
package nodeprovisiontest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// Server fakes a node's Incus API: GET and POST /1.0/instances,
// GET /1.0/operations/{id}/wait and DELETE /1.0/certificates/{fingerprint}.
// Set the knobs before the first request; read the recordings through the
// accessor methods, which lock.
//
// Like a real node, a revoke makes every later request answer 403, which is
// how a spent bootstrap cert looks to its next caller. It does not check
// which client cert is presented: one fake serves one credential.
type Server struct {
	srv *httptest.Server

	// CreateErr makes POST /1.0/instances return an Incus error.
	CreateErr bool
	// WaitStatus is the final status of the create operation; zero means
	// lxcapi.Success.
	WaitStatus lxcapi.StatusCode
	// OnWait, if set, runs first in each operation wait, e.g. to cancel the
	// caller's context while its create is in flight.
	OnWait func(r *http.Request)

	mu        sync.Mutex
	instances []string
	forbidden bool
	created   []lxcapi.InstancesPost
	revoked   []string
}

// New starts a Server with the given existing instance names; it closes on
// test cleanup.
func New(t *testing.T, instances ...string) *Server {
	t.Helper()
	f := &Server{instances: instances}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		if f.rejected(w) {
			return
		}
		f.mu.Lock()
		urls := make([]string, 0, len(f.instances))
		for _, name := range f.instances {
			urls = append(urls, "/1.0/instances/"+name)
		}
		f.mu.Unlock()
		meta, _ := json.Marshal(urls)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})
	mux.HandleFunc("POST /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		if f.rejected(w) {
			return
		}
		if f.CreateErr {
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Code: http.StatusInternalServerError, Error: "synthetic create failure"})
			return
		}
		var req lxcapi.InstancesPost
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Code: http.StatusBadRequest, Error: err.Error()})
			return
		}
		f.mu.Lock()
		f.created = append(f.created, req)
		f.instances = append(f.instances, req.Name)
		f.mu.Unlock()
		meta, _ := json.Marshal(lxcapi.Operation{ID: "OP1", StatusCode: lxcapi.Running})
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.AsyncResponse, Metadata: meta})
	})
	mux.HandleFunc("GET /1.0/operations/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		if f.OnWait != nil {
			f.OnWait(r)
		}
		op := lxcapi.Operation{ID: r.PathValue("id"), StatusCode: f.WaitStatus}
		if op.StatusCode == 0 {
			op.StatusCode = lxcapi.Success
		}
		if op.StatusCode == lxcapi.Failure {
			op.Err = "synthetic operation failure"
		}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})
	mux.HandleFunc("DELETE /1.0/certificates/{fingerprint}", func(w http.ResponseWriter, r *http.Request) {
		if f.rejected(w) {
			return
		}
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PathValue("fingerprint"))
		f.forbidden = true
		f.mu.Unlock()
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse})
	})

	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// rejected answers 403 the way Incus does for an untrusted client, once the
// cert has been revoked.
func (f *Server) rejected(w http.ResponseWriter) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.forbidden {
		return false
	}
	w.WriteHeader(http.StatusForbidden)
	writeEnvelope(w, lxcapi.Response{Type: lxcapi.ErrorResponse, Code: http.StatusForbidden, Error: "not authorized"})
	return true
}

func writeEnvelope(w http.ResponseWriter, resp lxcapi.Response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// Addr returns the server's host:port, e.g. "127.0.0.1:54321".
func (f *Server) Addr() string {
	return strings.TrimPrefix(f.srv.URL, "https://")
}

// Dial is a plain TCP nodeprovision.DialFunc, standing in for the WireGuard
// tunnel's or the LAN's.
func (f *Server) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// Created returns every InstancesPost the server accepted, in order.
func (f *Server) Created() []lxcapi.InstancesPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lxcapi.InstancesPost(nil), f.created...)
}

// Revoked returns every fingerprint revoked, in order.
func (f *Server) Revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}
