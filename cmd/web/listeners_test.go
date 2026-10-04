package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/store"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// captureLog redirects the standard logger for the test's duration.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// serve runs every listener until the test ends.
func serve(t *testing.T, ls []listener) {
	t.Helper()
	for _, l := range ls {
		go func() { _ = l.srv.Serve(l.ln) }()
	}
	t.Cleanup(func() {
		for _, l := range ls {
			_ = l.srv.Close()
		}
	})
}

// apiStub stands in for the real API handler: any route answers 200.
var apiStub = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = fmt.Fprintf(w, "api %s", r.URL.Path)
})

func get(t *testing.T, client *http.Client, url string) (int, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck // test
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func TestListenDefaultServesHealthOnly(t *testing.T) {
	t.Setenv("HEALTH_LISTEN_ADDR", "127.0.0.1:0")
	logs := captureLog(t)

	ls, err := listen(apiStub, nil, nil, "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if len(ls) != 1 {
		t.Fatalf("got %d listeners, want only the health listener", len(ls))
	}
	serve(t, ls)

	base := "http://" + ls[0].ln.Addr().String()
	if code, _, err := get(t, http.DefaultClient, base+"/healthz"); err != nil || code != http.StatusOK {
		t.Errorf("GET /healthz = %d, %v; want 200", code, err)
	}
	if code, _, err := get(t, http.DefaultClient, base+"/status"); err != nil || code != http.StatusNotFound {
		t.Errorf("GET /status on the health listener = %d, %v; want 404 (no API route)", code, err)
	}
	if !strings.Contains(logs.String(), "not served on any listener") {
		t.Errorf("no warning that the API is unserved; logs:\n%s", logs)
	}
}

func TestListenHostListenerServesAPIAndWarns(t *testing.T) {
	t.Setenv("HEALTH_LISTEN_ADDR", "127.0.0.1:0")
	logs := captureLog(t)

	ls, err := listen(apiStub, nil, nil, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if len(ls) != 2 {
		t.Fatalf("got %d listeners, want health + host", len(ls))
	}
	serve(t, ls)

	code, body, err := get(t, http.DefaultClient, "http://"+ls[1].ln.Addr().String()+"/status")
	if err != nil || code != http.StatusOK || body != "api /status" {
		t.Errorf("GET /status on the host listener = %d %q, %v; want 200 from the API", code, body, err)
	}
	for _, want := range []string{"WARNING: API_HOST_LISTEN_ADDR=", "WireGuard private key"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("startup log lacks %q; logs:\n%s", want, logs)
		}
	}
}

func TestListenRejectsABadHostAddress(t *testing.T) {
	t.Setenv("HEALTH_LISTEN_ADDR", "127.0.0.1:0")
	captureLog(t)
	if _, err := listen(apiStub, nil, nil, "not-an-address"); err == nil {
		t.Fatal("listen with an unparseable API_HOST_LISTEN_ADDR succeeded, want an error")
	}
}

// TestListenTunnelAdmitsOperatorsAndRefusesNodes drives the default posture
// through real tunnels: the web app's tunnel (newTunnelSource, with one
// operator peer) serves the API on WebAppAddr, an operator peer reaches it,
// and a node peer, registered the way a sync registers instances, is refused.
func TestListenTunnelAdmitsOperatorsAndRefusesNodes(t *testing.T) {
	t.Setenv("HEALTH_LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("WIREGUARD_PORT", "0")
	captureLog(t)

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	opPriv, opPub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	opAddr := netip.MustParseAddr("10.100.0.240")
	operators := []wireguard.OperatorPeer{{PublicKey: opPub, Addr: opAddr}}

	ts, err := newTunnelSource(context.Background(), st, "127.0.0.1:51820", operators)
	if err != nil {
		t.Fatalf("newTunnelSource: %v", err)
	}
	defer ts.Close() //nolint:errcheck // test cleanup
	appPort, err := ts.tun.ListenPort()
	if err != nil {
		t.Fatalf("ListenPort: %v", err)
	}

	nodePriv, nodePub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	nodeAddr := netip.MustParseAddr("10.100.0.2")
	if err := ts.UpsertPeer(nodePub, nodeAddr); err != nil {
		t.Fatalf("register node peer: %v", err)
	}

	ls, err := listen(apiStub, ts.tun, operators, "")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if len(ls) != 2 {
		t.Fatalf("got %d listeners, want health + tunnel", len(ls))
	}
	serve(t, ls)

	endpoint := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(appPort)) //nolint:gosec // G115: a UDP port fits in uint16
	peer := func(priv wireguard.PrivateKey, local netip.Addr) *http.Client {
		tun, err := wireguard.Start(wireguard.Options{PrivateKey: priv, ListenPort: 0, LocalAddr: local})
		if err != nil {
			t.Fatalf("start peer tunnel: %v", err)
		}
		t.Cleanup(func() { _ = tun.Close() })
		if err := tun.UpsertPeerWithEndpoint(ts.PublicKey(), wireguard.WebAppAddr, endpoint); err != nil {
			t.Fatalf("peer trusts web app: %v", err)
		}
		return &http.Client{Transport: &http.Transport{DialContext: tun.DialContext}}
	}
	url := fmt.Sprintf("http://%s/status", net.JoinHostPort(wireguard.WebAppAddr.String(), fmt.Sprint(wireguard.APIPort)))

	// The first request also waits out the handshake, so retry it.
	retry := func(client *http.Client) (int, string) {
		var lastErr error
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			code, body, err := get(t, client, url)
			if err == nil {
				return code, body
			}
			lastErr = err
		}
		t.Fatalf("GET %s never answered through the tunnel: %v", url, lastErr)
		return 0, ""
	}

	if code, body := retry(peer(opPriv, opAddr)); code != http.StatusOK || body != "api /status" {
		t.Errorf("operator peer: GET /status = %d %q, want 200 from the API", code, body)
	}
	if code, _ := retry(peer(nodePriv, nodeAddr)); code != http.StatusForbidden {
		t.Errorf("node peer: GET /status = %d, want 403", code)
	}
}
