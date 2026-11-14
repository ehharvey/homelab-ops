package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/deployconfig"
	"github.com/ehharvey/homelab-ops/internal/server"
	"github.com/ehharvey/homelab-ops/internal/store"
	"github.com/ehharvey/homelab-ops/internal/webtls"
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

// operatorCert is a fresh break-glass-style client cert, as `bootstrap
// gen-cert` makes one, ready to present and to list.
func operatorCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	pair, err := cert.Generate(cert.Options{CommonName: "operator", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	tc, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(tc.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return tc, leaf
}

// testAPITLS is newAPITLS for a fresh identity, and the pin a client needs.
func testAPITLS(t *testing.T) (*tls.Config, string) {
	t.Helper()
	priv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := newAPITLS(priv, time.Now())
	if err != nil {
		t.Fatalf("newAPITLS: %v", err)
	}
	key, err := webtls.DeriveKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := webtls.PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, pin
}

// pinnedClient is the CLI's TLS client over dial, presenting cert (nil: none).
func pinnedClient(t *testing.T, dial apiclient.DialFunc, pin string, cert *tls.Certificate) *http.Client {
	t.Helper()
	c, err := apiclient.PinnedHTTPClient(dial, apiclient.TLSOptions{ClientCert: cert, ServerPin: pin})
	if err != nil {
		t.Fatalf("PinnedHTTPClient: %v", err)
	}
	return c
}

func TestListenDefaultServesHealthOnly(t *testing.T) {
	logs := captureLog(t)

	ls, err := listen(apiStub, listenOptions{healthAddr: "127.0.0.1:0"})
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

func TestListenPlainHostListenerServesAPIAndWarns(t *testing.T) {
	logs := captureLog(t)

	ls, err := listen(apiStub, listenOptions{healthAddr: "127.0.0.1:0", hostAddr: "127.0.0.1:0"})
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
	for _, want := range []string{"WARNING: API_HOST_LISTEN_ADDR=", "API_HOST_PLAIN_HTTP=true", "WireGuard private key"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("startup log lacks %q; logs:\n%s", want, logs)
		}
	}
}

// isNotOperator reports whether a GET answered webtls.RequireOperator's 403.
func isNotOperator(code int, body string, err error) bool {
	return err == nil && code == http.StatusForbidden && strings.TrimSpace(body) == webtls.NotOperatorMessage
}

// The host listener without the plain-HTTP opt-in (#397): TLS, and only a
// listed operator cert gets in, the others getting 403. It has no other
// gate.
func TestListenTLSHostListenerRequiresAnOperatorCert(t *testing.T) {
	logs := captureLog(t)
	opCert, opLeaf := operatorCert(t)
	otherCert, _ := operatorCert(t)
	apiTLS, pin := testAPITLS(t)

	ls, err := listen(apiStub, listenOptions{healthAddr: "127.0.0.1:0", hostAddr: "127.0.0.1:0", hostTLS: true, tls: apiTLS, operatorCerts: []*x509.Certificate{opLeaf}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serve(t, ls)
	if strings.Contains(logs.String(), "WARNING: API_HOST_LISTEN_ADDR") {
		t.Errorf("a TLS host listener logged the plain-HTTP warning:\n%s", logs)
	}

	addr := ls[1].ln.Addr().String()
	var d net.Dialer
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	url := "https://" + addr + "/status"

	if code, body, err := get(t, pinnedClient(t, dial, pin, &opCert), url); err != nil || code != http.StatusOK || body != "api /status" {
		t.Errorf("operator cert: GET /status = %d %q, %v; want 200 from the API", code, body, err)
	}
	for name, c := range map[string]*tls.Certificate{"no client cert": nil, "an unlisted cert": &otherCert} {
		if code, body, err := get(t, pinnedClient(t, dial, pin, c), url); !isNotOperator(code, body, err) {
			t.Errorf("%s: GET /status = %d %q, %v; want 403 %q", name, code, body, err, webtls.NotOperatorMessage)
		}
		// Every route, not only the API's own: an unknown one is 403 too.
		if code, _, err := get(t, pinnedClient(t, dial, pin, c), "https://"+addr+"/no-such-route"); err != nil || code != http.StatusForbidden {
			t.Errorf("%s: GET /no-such-route = %d, %v; want 403", name, code, err)
		}
	}
	if code, _, err := get(t, http.DefaultClient, "http://"+addr+"/status"); err == nil && code == http.StatusOK {
		t.Error("plain HTTP on the TLS host listener got 200")
	}
	_, wrongPin := testAPITLS(t)
	_, _, err = get(t, pinnedClient(t, dial, wrongPin, &opCert), url)
	var mismatch *apiclient.PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("a wrong pin: err = %v, want the client's PinMismatchError", err)
	}
}

func TestListenRefusesTLSListenersWithoutATLSConfig(t *testing.T) {
	captureLog(t)
	if _, err := listen(apiStub, listenOptions{healthAddr: "127.0.0.1:0", hostAddr: "127.0.0.1:0", hostTLS: true}); err == nil {
		t.Fatal("listen with a TLS host listener and no TLS config succeeded, want an error")
	}
}

func TestListenRejectsABadHostAddress(t *testing.T) {
	captureLog(t)
	if _, err := listen(apiStub, listenOptions{healthAddr: "127.0.0.1:0", hostAddr: "not-an-address"}); err == nil {
		t.Fatal("listen with an unparseable API_HOST_LISTEN_ADDR succeeded, want an error")
	}
}

// TestListenTunnelAdmitsOperatorsAndRefusesNodes drives the default posture
// through real tunnels: the web app's tunnel (newTunnelSource, with one
// operator peer that startup's RestoreTunnelPeers registers) serves the API
// over TLS on WebAppAddr, an operator peer presenting the operator cert
// reaches it, one presenting none gets the cert check's 403, and a node
// peer, registered the way a sync registers instances, gets the peer
// filter's 403 even with the operator cert: the peer gate still applies
// (#397).
func TestListenTunnelAdmitsOperatorsAndRefusesNodes(t *testing.T) {
	captureLog(t)
	opCert, opLeaf := operatorCert(t)

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

	ts, err := newTunnelSource(context.Background(), st, "127.0.0.1:51820", 0, operators)
	if err != nil {
		t.Fatalf("newTunnelSource: %v", err)
	}
	defer ts.Close() //nolint:errcheck // test cleanup
	if err := server.NewService(nil, st, nil, nil, ts, st).RestoreTunnelPeers(context.Background()); err != nil {
		t.Fatalf("RestoreTunnelPeers: %v", err)
	}
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

	apiTLS, err := newAPITLS(ts.identity, time.Now())
	if err != nil {
		t.Fatalf("newAPITLS: %v", err)
	}
	key, err := webtls.DeriveKey(ts.identity)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := webtls.PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	ls, err := listen(apiStub, listenOptions{tun: ts.tun, operators: operators, healthAddr: "127.0.0.1:0", tls: apiTLS, operatorCerts: []*x509.Certificate{opLeaf}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if len(ls) != 2 {
		t.Fatalf("got %d listeners, want health + tunnel", len(ls))
	}
	serve(t, ls)

	endpoint := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(appPort)) //nolint:gosec // G115: a UDP port fits in uint16
	// peer joins the tunnel with priv at local and returns its dialer,
	// retried while the WireGuard handshake completes, as the CLI's is. One
	// tunnel per key: a second tunnel with the same key would race the
	// first for the web app's roamed endpoint.
	peer := func(priv wireguard.PrivateKey, local netip.Addr) apiclient.DialFunc {
		tun, err := wireguard.Start(wireguard.Options{PrivateKey: priv, ListenPort: 0, LocalAddr: local})
		if err != nil {
			t.Fatalf("start peer tunnel: %v", err)
		}
		t.Cleanup(func() { _ = tun.Close() })
		if err := tun.UpsertPeerWithEndpoint(ts.PublicKey(), wireguard.WebAppAddr, endpoint); err != nil {
			t.Fatalf("peer trusts web app: %v", err)
		}
		return apiclient.RetryDial(tun.DialContext, 20*time.Second, 250*time.Millisecond)
	}
	url := fmt.Sprintf("https://%s/status", net.JoinHostPort(wireguard.WebAppAddr.String(), fmt.Sprint(wireguard.APIPort)))
	opDial := peer(opPriv, opAddr)

	if code, body, err := get(t, pinnedClient(t, opDial, pin, &opCert), url); err != nil || code != http.StatusOK || body != "api /status" {
		t.Errorf("operator peer, operator cert: GET /status = %d %q, %v; want 200 from the API", code, body, err)
	}
	if code, body, err := get(t, pinnedClient(t, opDial, pin, nil), url); !isNotOperator(code, body, err) {
		t.Errorf("operator peer, no client cert: GET /status = %d %q, %v; want 403 %q", code, body, err, webtls.NotOperatorMessage)
	}
	if code, body, err := get(t, pinnedClient(t, peer(nodePriv, nodeAddr), pin, &opCert), url); err != nil || code != http.StatusForbidden || !strings.Contains(body, "not an operator peer") {
		t.Errorf("node peer, operator cert: GET /status = %d %q, %v; want the peer filter's 403", code, body, err)
	}
}

// With the tunnel on, a secrets/wireguard.key that isn't the tunnel's identity
// is warned about as a fingerprint that would change, and one that doesn't
// parse as what it is, without the fingerprint warning that would hide it.
func TestWarnUnreadWireGuardKey(t *testing.T) {
	identity, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		contents *string // nil: no file
		want     string  // "": no warning
		dontWant string
	}{
		{"no file", nil, "", ""},
		{"the tunnel's own identity", ptr(identity.Base64() + "\n"), "", ""},
		{"another identity", ptr(other.Base64() + "\n"), "its fingerprint would change", "can't be"},
		{"a malformed key", ptr("not-a-key\n"), "can't be", "would change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			dir := t.TempDir()
			if tc.contents != nil {
				if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, deployconfig.WireGuardKeyFile), []byte(*tc.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			warnUnreadWireGuardKey(dir, identity)
			got := logs.String()
			switch {
			case tc.want == "" && got != "":
				t.Errorf("logged %q, want nothing", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("log %q lacks %q", got, tc.want)
			case tc.dontWant != "" && strings.Contains(got, tc.dontWant):
				t.Errorf("log %q says %q", got, tc.dontWant)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// A set PORT is only logged here: deployconfig.Load refuses one with no API
// listener (TestLoadRefusesRetiredPortWithoutAListener).
func TestNoteRetiredPort(t *testing.T) {
	cases := []struct {
		name, port string
		wantLog    bool
	}{
		{"PORT unset", "", false},
		{"PORT set", "8080", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			noteRetiredPort(tc.port)
			if got := strings.Contains(logs.String(), "is no longer read"); got != tc.wantLog {
				t.Errorf("logged the retired-PORT notice: %v, want %v (log: %q)", got, tc.wantLog, logs.String())
			}
		})
	}
}

// A listener still draining must not keep the others accepting: shutdown
// closes them all at once.
func TestShutdownStopsEveryListenerAtOnce(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	slow := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})
	var ls []listener
	for _, h := range []http.Handler{slow, apiStub} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		ls = append(ls, listener{name: ln.Addr().String(), ln: ln, srv: &http.Server{Handler: h, ReadHeaderTimeout: time.Second}})
	}
	serve(t, ls)

	go func() { _, _, _ = get(t, http.DefaultClient, "http://"+ls[0].ln.Addr().String()+"/") }()
	<-started

	done := make(chan error, 1)
	go func() { done <- shutdown(ls, 10*time.Second) }()

	second := ls[1].ln.Addr().String()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", second, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the second listener still accepts while the first drains")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(release)
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
