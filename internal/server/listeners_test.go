package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func TestOperatorsOnlyAdmitsOnlyOperatorAddresses(t *testing.T) {
	operators := []wireguard.OperatorPeer{{Addr: addr("10.100.0.240")}, {Addr: addr("10.100.0.241")}}
	reached := 0
	h := OperatorsOnly(operators, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusTeapot)
	}))

	cases := []struct {
		name, remote string
		want         int
	}{
		{"first operator", "10.100.0.240:41000", http.StatusTeapot},
		{"second operator", "10.100.0.241:41000", http.StatusTeapot},
		{"IPv4-mapped operator", "[::ffff:10.100.0.240]:41000", http.StatusTeapot},
		{"node peer", "10.100.0.2:41000", http.StatusForbidden},
		{"unlisted operator-range address", "10.100.0.242:41000", http.StatusForbidden},
		{"web app's own address", "10.100.0.1:41000", http.StatusForbidden},
		{"unparseable", "not-an-addr", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := reached
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.RemoteAddr = tc.remote
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if forbidden := tc.want == http.StatusForbidden; forbidden && reached != before {
				t.Error("a refused request reached the wrapped handler")
			}
		})
	}
}

func TestOperatorsOnlyWithNoOperatorsRefusesEveryone(t *testing.T) {
	h := OperatorsOnly(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached with no operator peers configured")
	}))
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "10.100.0.240:41000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestHealthHandlerServesOnlyHealthz(t *testing.T) {
	h := NewHealthHandler()
	for path, want := range map[string]int{
		"/healthz":   http.StatusOK,
		"/status":    http.StatusNotFound,
		"/instances": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

// TestReconcileTunnelPeersKeepsOperatorPeers: a sync must leave the operator
// peers registered, not prune the peer table down to instances (#195). They
// are upserted after the instances, so an operator's address always stays
// with the operator's key.
func TestReconcileTunnelPeersKeepsOperatorPeers(t *testing.T) {
	_, opPub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	op := wireguard.OperatorPeer{PublicKey: opPub, Addr: addr("10.100.0.240")}
	tunnels := &fakeTunnelSource{operators: []wireguard.OperatorPeer{op}}
	instances := []config.Instance{
		{Name: "node-a", TunnelIP: addr("10.100.0.2")},
		{Name: "node-b", TunnelIP: addr("10.100.0.3")},
	}

	reconcileTunnelPeers(context.Background(), tunnels, &fakeCredentialStore{}, instances)

	if len(tunnels.upsertCall) != 3 {
		t.Fatalf("len(upsertCall) = %d, want 3 (two instances, then the operator)", len(tunnels.upsertCall))
	}
	last := tunnels.upsertCall[2]
	if last.pub != op.PublicKey || last.tunnelIP != op.Addr {
		t.Errorf("last upsert = %s at %s, want the operator %s at %s", last.pub, last.tunnelIP, op.PublicKey, op.Addr)
	}
}

func TestRefusalLogLimitsEachAddress(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRefusalLog(func() time.Time { return now }, time.Minute)
	node, other := addr("10.100.0.2"), addr("10.100.0.3")

	if _, ok := l.allow(node); !ok {
		t.Fatal("first refusal from an address isn't logged")
	}
	for range 3 {
		if _, ok := l.allow(node); ok {
			t.Fatal("a repeat refusal within the interval is logged")
		}
	}
	if _, ok := l.allow(other); !ok {
		t.Fatal("another address's first refusal is held back by the first address")
	}

	now = now.Add(time.Minute)
	suppressed, ok := l.allow(node)
	if !ok || suppressed != 3 {
		t.Fatalf("after the interval: allow = (%d, %v), want (3, true)", suppressed, ok)
	}
	if _, ok := l.allow(node); ok {
		t.Fatal("the interval didn't restart after the logged line")
	}
}

func TestLogMovedTunnelIPsNamesReassignedInstances(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	prior := []config.Instance{
		{Name: "kept", TunnelIP: addr("10.100.0.2")},
		{Name: "moved", TunnelIP: addr("10.100.0.240")},
	}
	instances := []config.Instance{
		{Name: "kept", TunnelIP: addr("10.100.0.2")},
		{Name: "moved", TunnelIP: addr("10.100.0.3")},
		{Name: "new", TunnelIP: addr("10.100.0.4")},
	}
	logMovedTunnelIPs(instances, prior)

	out := buf.String()
	if !strings.Contains(out, `instance "moved"'s tunnel address moved from 10.100.0.240`) || !strings.Contains(out, "to 10.100.0.3") {
		t.Errorf("no warning for the moved instance: %q", out)
	}
	if strings.Contains(out, `"kept"`) || strings.Contains(out, `"new"`) {
		t.Errorf("warned about an instance whose address didn't move: %q", out)
	}
}
