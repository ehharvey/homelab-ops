package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/webtls"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// resetAPIFlags puts the api group's flag variables back to their defaults:
// cobra only assigns the flags a run passes, so values leak between runs.
func resetAPIFlags(t *testing.T) {
	t.Helper()
	apiFlags.privateKeyFile = ""
	apiFlags.localAddr = ""
	apiFlags.serverPublicKey = ""
	apiFlags.endpoint = ""
	apiFlags.serverAddr = wireguard.WebAppAddr.String()
	apiFlags.listenPort = 0
	apiFlags.connectTimeout = 30 * time.Second
	apiFlags.clientCert = ""
	apiFlags.clientKey = ""
	apiFlags.serverFingerprint = ""
	apiFlags.pinsFile = ""
	apiFlags.repin = false
	apiSeedOutput = ""
	apiImageOutput = ""
}

// fakeAPI points apiConnect at h over httptest for the rest of the test and
// reports how many times a verb connected.
func fakeAPI(t *testing.T, h http.Handler) *int {
	t.Helper()
	resetAPIFlags(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var connects int
	orig := apiConnect
	apiConnect = func(context.Context) (*apiclient.Client, func(), error) {
		connects++
		c, err := apiclient.New(srv.URL, srv.Client())
		return c, func() {}, err
	}
	t.Cleanup(func() { apiConnect = orig; resetAPIFlags(t) })
	return &connects
}

func runAPI(args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	rootCmd.SetArgs(args)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

func TestAPIJSONVerbsPrintTheServersAnswer(t *testing.T) {
	routes := map[string]string{
		"GET /status":                        `{"synced":true,"commit":"abc"}`,
		"GET /networks":                      `[{"Name":"lan"}]`,
		"GET /instances":                     `[{"Name":"node0"}]`,
		"POST /sync":                         `{"commit":"abc","networks":1,"instances":1}`,
		"POST /instances/node0/deploy-agent": `{"status":"deployed","instance":"agent-node0"}`,
	}
	mux := http.NewServeMux()
	for pattern, body := range routes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
	}
	fakeAPI(t, mux)

	for _, tc := range []struct {
		args    []string
		pattern string
	}{
		{[]string{"api", "status"}, "GET /status"},
		{[]string{"api", "networks"}, "GET /networks"},
		{[]string{"api", "instances"}, "GET /instances"},
		{[]string{"api", "sync"}, "POST /sync"},
		{[]string{"api", "deploy-agent", "node0"}, "POST /instances/node0/deploy-agent"},
	} {
		out, _, err := runAPI(tc.args...)
		if err != nil {
			t.Errorf("%v: %v", tc.args, err)
			continue
		}
		if want := routes[tc.pattern] + "\n"; out != want {
			t.Errorf("%v printed %q, want %q", tc.args, out, want)
		}
	}
}

func TestAPIErrorExitsWithTheServersMessage(t *testing.T) {
	fakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sync failed", http.StatusBadGateway)
	}))
	out, stderr, err := runAPI("api", "sync")
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502 *apiclient.APIError", err)
	}
	if !strings.Contains(stderr, "sync failed") {
		t.Errorf("stderr = %q, want the server's message", stderr)
	}
	if strings.Contains(stderr, "Usage:") {
		t.Errorf("an API error printed usage: %q", stderr)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
}

func TestAPIVerbsCheckTheirArgs(t *testing.T) {
	connects := fakeAPI(t, http.NotFoundHandler())
	for _, args := range [][]string{
		{"api", "status", "extra"},
		{"api", "seed"},
		{"api", "image"},
		{"api", "deploy-agent"},
		{"api", "deploy-agent", "a", "b"},
		{"api", "seed", "node0"},  // no --output
		{"api", "image", "node0"}, // no --output
	} {
		resetAPIFlags(t)
		if _, _, err := runAPI(args...); err == nil {
			t.Errorf("%v succeeded, want an error", args)
		}
	}
	if *connects != 0 {
		t.Errorf("bad args connected %d times, want 0", *connects)
	}
}

func TestAPISeedSavesTheAnswerPrivately(t *testing.T) {
	const body = `{"install_yaml":"a","network_yaml":"private_key: x"}`
	fakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/instances/node0/seed" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	path := filepath.Join(t.TempDir(), "seed.json")
	out, _, err := runAPI("api", "seed", "node0", "--output", path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want the seed only in the file", out)
	}
	got, err := os.ReadFile(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body+"\n" {
		t.Errorf("file = %q, want %q", got, body+"\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	resetAPIFlags(t)
	out, _, err = runAPI("api", "seed", "node0", "-o", "-")
	if err != nil || out != body+"\n" {
		t.Errorf("seed -o -: printed %q, err %v; want the seed on stdout", out, err)
	}
}

func TestAPISeedErrorWritesNoFile(t *testing.T) {
	fakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	path := filepath.Join(t.TempDir(), "seed.json")
	_, _, err := runAPI("api", "seed", "ghost", "--output", path)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a failed seed left %s behind (stat: %v)", path, err)
	}
}

func TestAPIImageStreamsToTheFile(t *testing.T) {
	const size = 8 << 20
	fakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/instances/node0/image" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = io.CopyN(w, zeroReader{}, size)
	}))
	path := filepath.Join(t.TempDir(), "node0.img")
	_, stderr, err := runAPI("api", "image", "node0", "--output", path)
	if err != nil {
		t.Fatalf("image: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size || info.Mode().Perm() != 0o600 {
		t.Errorf("file is %d bytes, mode %o; want %d bytes, mode 600", info.Size(), info.Mode().Perm(), size)
	}
	if !strings.Contains(stderr, strconv.Itoa(size)+" bytes") {
		t.Errorf("stderr = %q, want the byte count", stderr)
	}
}

func TestAPIImageCutShortRemovesThePartialFile(t *testing.T) {
	fakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(make([]byte, 100))
	}))
	path := filepath.Join(t.TempDir(), "node0.img")
	if _, _, err := runAPI("api", "image", "node0", "--output", path); err == nil {
		t.Fatal("a truncated image download succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the partial image is still at %s (stat: %v)", path, err)
	}
}

func TestAPIImageErrorKeepsAnExistingFile(t *testing.T) {
	fakeAPI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "image generation not configured (set BASE_IMAGE_PATH)", http.StatusServiceUnavailable)
	}))
	path := filepath.Join(t.TempDir(), "node0.img")
	if err := os.WriteFile(path, []byte("older image"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runAPI("api", "image", "node0", "--output", path)
	if err == nil || !strings.Contains(stderr, "image generation not configured") {
		t.Errorf("err = %v, stderr %q; want the server's 503 message", err, stderr)
	}
	// The request failed before any byte arrived, so the file was never opened.
	if got, _ := os.ReadFile(path); string(got) != "older image" { //nolint:gosec // test temp file
		t.Errorf("file = %q, want it untouched", got)
	}
}

// TestAPIOutputCheckedBeforeTheRequest: an --output that can't be written
// fails before the CLI connects, so the web app never builds an image (or a
// seed) only for the write to fail.
func TestAPIOutputCheckedBeforeTheRequest(t *testing.T) {
	connects := fakeAPI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request was sent")
	}))
	bad := filepath.Join(t.TempDir(), "no-such-dir", "node0.img")
	for _, verb := range []string{"image", "seed"} {
		_, _, err := runAPI("api", verb, "node0", "--output", bad)
		if err == nil || !strings.Contains(err.Error(), "open --output") {
			t.Errorf("api %s: err = %v, want open --output", verb, err)
		}
	}
	if *connects != 0 {
		t.Errorf("connected %d times, want none", *connects)
	}
	if _, err := os.Stat(filepath.Dir(bad)); !os.IsNotExist(err) {
		t.Errorf("the check created %s (stat: %v)", filepath.Dir(bad), err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func writeKeyFile(t *testing.T, priv wireguard.PrivateKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "op.key")
	if err := os.WriteFile(path, []byte(priv.Base64()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPITunnelOptionsChecksFlags(t *testing.T) {
	priv, _, _ := wireguard.GenerateKeypair()
	_, srvPub, _ := wireguard.GenerateKeypair()
	keyFile := writeKeyFile(t, priv)
	good := func() {
		resetAPIFlags(t)
		apiFlags.privateKeyFile = keyFile
		apiFlags.localAddr = "10.100.0.240"
		apiFlags.serverPublicKey = srvPub.Base64()
		apiFlags.endpoint = "192.0.2.1:51820"
	}
	t.Cleanup(func() { resetAPIFlags(t) })

	good()
	opts, endpoints, baseURL, err := apiTunnelOptions(context.Background())
	if err != nil {
		t.Fatalf("good flags: %v", err)
	}
	if baseURL != "https://10.100.0.1:80" {
		t.Errorf("baseURL = %q, want https://10.100.0.1:80", baseURL)
	}
	if opts.PrivateKey != priv || opts.PeerPublicKey != srvPub ||
		opts.LocalAddr != netip.MustParseAddr("10.100.0.240") ||
		opts.PeerTunnelIP != wireguard.WebAppAddr {
		t.Errorf("opts = %+v, not what the flags said", opts)
	}
	if len(endpoints) != 1 || endpoints[0] != netip.MustParseAddrPort("192.0.2.1:51820") {
		t.Errorf("endpoints = %v, want [192.0.2.1:51820]", endpoints)
	}

	resetAPIFlags(t)
	_, _, _, err = apiTunnelOptions(context.Background())
	for _, f := range []string{"--private-key-file", "--local-addr", "--server-public-key", "--endpoint"} {
		if err == nil || !strings.Contains(err.Error(), f) {
			t.Errorf("no flags: err = %v, want it to name %s", err, f)
		}
	}

	for name, spoil := range map[string]func(){
		"missing key file": func() { apiFlags.privateKeyFile = filepath.Join(t.TempDir(), "nope") },
		"bad local addr":   func() { apiFlags.localAddr = "10.100.0" },
		"bad server key":   func() { apiFlags.serverPublicKey = "not-base64" },
		"bad server addr":  func() { apiFlags.serverAddr = "web" },
		"bad endpoint":     func() { apiFlags.endpoint = "192.0.2.1" },
		"bad listen port":  func() { apiFlags.listenPort = 70000 },
		"zero timeout":     func() { apiFlags.connectTimeout = 0 },
	} {
		good()
		spoil()
		if _, _, _, err := apiTunnelOptions(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestAPITLSOptionsChecksFlags: the cert and key go together and must load,
// and a --server-fingerprint must be one.
func TestAPITLSOptionsChecksFlags(t *testing.T) {
	resetAPIFlags(t)
	t.Cleanup(func() { resetAPIFlags(t) })
	certFile, keyFile, _ := writeOperatorCert(t)

	apiFlags.clientCert, apiFlags.clientKey, apiFlags.pinsFile = certFile, keyFile, "/pins"
	o, err := apiTLSOptions()
	if err != nil || o.clientCert == nil || o.pinsFile != "/pins" {
		t.Errorf("good flags: %+v, %v", o, err)
	}
	for name, spoil := range map[string]func(){
		"cert without key":  func() { apiFlags.clientKey = "" },
		"key without cert":  func() { apiFlags.clientCert = "" },
		"unreadable cert":   func() { apiFlags.clientCert = filepath.Join(t.TempDir(), "nope") },
		"bad fingerprint":   func() { apiFlags.serverFingerprint = "sha256//short" },
		"key for no cert":   func() { apiFlags.clientKey = certFile },
		"not a fingerprint": func() { apiFlags.serverFingerprint = "AA:BB" },
		"--repin with --server-fingerprint": func() {
			apiFlags.repin, apiFlags.serverFingerprint = true, "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
		},
		// Silently ignoring the file would leave a script believing it
		// checked or recorded the pin there (#397 review).
		"--pins-file with --server-fingerprint": func() {
			apiFlags.serverFingerprint = "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
		},
	} {
		apiFlags.clientCert, apiFlags.clientKey, apiFlags.serverFingerprint, apiFlags.repin, apiFlags.pinsFile = certFile, keyFile, "", false, "/pins"
		spoil()
		if _, err := apiTLSOptions(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// writeOperatorCert writes a gen-cert-style pair and returns its files and
// the parsed cert.
func writeOperatorCert(t *testing.T) (certFile, keyFile string, leaf *x509.Certificate) {
	t.Helper()
	pair, err := cert.Generate(cert.Options{CommonName: "operator", ValidityDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile, err = cert.Write(dir, pair, false)
	if err != nil {
		t.Fatal(err)
	}
	certs, err := webtls.ParseCertsPEM(pair.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, certs[0]
}

// testWebApp is a tunnel standing in for the web app's (startTestWebAppTunnel).
type testWebApp struct {
	pub     wireguard.PublicKey
	port    int
	cliPriv wireguard.PrivateKey
	cliAddr netip.Addr
	// pin is its TLS key's fingerprint; certFile and keyFile are the
	// operator cert it requires.
	pin, certFile, keyFile string
}

// startTestWebAppTunnel starts a tunnel standing in for the web app's, on
// loopback, that lists one operator peer and serves GET /status on the API
// port over TLS, with a key derived from its identity and requiring one
// operator cert, as the web app does (#397). It answers with the caller's
// tunnel address.
func startTestWebAppTunnel(t *testing.T) testWebApp {
	t.Helper()
	srvPriv, srvPub, _ := wireguard.GenerateKeypair()
	srv, err := wireguard.Start(wireguard.Options{PrivateKey: srvPriv, LocalAddr: wireguard.WebAppAddr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() }) //nolint:errcheck,gosec // test cleanup
	srvPort, err := srv.ListenPort()
	if err != nil {
		t.Fatal(err)
	}
	cliPriv, cliPub, _ := wireguard.GenerateKeypair()
	cliAddr := netip.MustParseAddr("10.100.0.240")
	if err := srv.UpsertPeer(cliPub, cliAddr); err != nil {
		t.Fatal(err)
	}
	ln, err := srv.Listen(wireguard.APIPort)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, leaf := writeOperatorCert(t)
	key, err := webtls.DeriveKey(srvPriv)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := webtls.SelfSigned(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pin, err := webtls.PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ln = tls.NewListener(ln, webtls.ServerConfig(serverCert))
	hs := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: webtls.RequireOperator([]*x509.Certificate{leaf}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"synced":true,"from":"`+r.RemoteAddr+`"}`)
	}))}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { hs.Close() }) //nolint:errcheck,gosec // test cleanup
	return testWebApp{pub: srvPub, port: srvPort, cliPriv: cliPriv, cliAddr: cliAddr, pin: pin, certFile: certFile, keyFile: keyFile}
}

// TestStartFirstAnsweringTriesEachEndpoint: of an endpoint name's addresses,
// one that never answers is skipped for the next, and when none answers the
// error names each address.
func TestStartFirstAnsweringTriesEachEndpoint(t *testing.T) {
	app := startTestWebAppTunnel(t)
	srvPub, srvPort, cliPriv, cliAddr := app.pub, app.port, app.cliPriv, app.cliAddr
	// A UDP port with nothing behind it: the handshake never completes.
	deadConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := netip.MustParseAddrPort(deadConn.LocalAddr().String())
	deadConn.Close() //nolint:errcheck,gosec // freed so nothing answers there

	live := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(srvPort)) //nolint:gosec // G115: a UDP port
	opts := wireguard.ClientOptions{PrivateKey: cliPriv, LocalAddr: cliAddr, PeerPublicKey: srvPub, PeerTunnelIP: wireguard.WebAppAddr}
	ctx := context.Background()

	tun, dial, err := startFirstAnswering(ctx, opts, []netip.AddrPort{dead, live}, 2*time.Second)
	if err != nil {
		t.Fatalf("dead then live: %v", err)
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(wireguard.WebAppAddr.String(), strconv.Itoa(int(wireguard.APIPort))))
	if err != nil {
		t.Errorf("dial after connecting: %v", err)
	} else {
		conn.Close() //nolint:errcheck,gosec // test
	}
	tun.Close() //nolint:errcheck,gosec // test

	_, _, err = startFirstAnswering(ctx, opts, []netip.AddrPort{dead}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "endpoint "+dead.String()+": no answer from") {
		t.Errorf("only dead: err = %v, want it to name %s", err, dead)
	}
}

// TestAPIStatusOverARealTunnel runs `bootstrap api status` with only flags,
// through the CLI's own WireGuard peer, against a tunnel standing in for the
// web app's: what scripts/validate/operator-cli-drives-api-over-tunnel.sh
// does against the real one. The operator cert and the pin are flags, as a
// script passes them.
func TestAPIStatusOverARealTunnel(t *testing.T) {
	resetAPIFlags(t)
	t.Cleanup(func() { resetAPIFlags(t) })
	app := startTestWebAppTunnel(t)

	out, _, err := runAPI(app.statusArgs(t, "--server-fingerprint", app.pin)...)
	if err != nil {
		t.Fatalf("api status: %v", err)
	}
	if !strings.Contains(out, `"synced":true`) || !strings.Contains(out, `"from":"10.100.0.240:`) {
		t.Errorf("printed %q, want the status, seen from the operator address", out)
	}

	resetAPIFlags(t)
	other := startTestWebAppTunnel(t)
	_, _, err = runAPI(app.statusArgs(t, "--server-fingerprint", other.pin)...)
	var mismatch *apiclient.PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("a wrong --server-fingerprint: err = %v, want the pin refused", err)
	}

	// No operator cert: the web app's 403 comes with what to pass.
	resetAPIFlags(t)
	var noCert []string
	args := app.statusArgs(t, "--server-fingerprint", app.pin)
	for i := 0; i < len(args); i++ {
		if args[i] == "--client-cert" || args[i] == "--client-key" {
			i++
			continue
		}
		noCert = append(noCert, args[i])
	}
	_, _, err = runAPI(noCert...)
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || !strings.Contains(err.Error(), "pass the operator cert with --client-cert and --client-key") {
		t.Errorf("no client cert: err = %v, want the 403 explained", err)
	}

	// A cert the web app doesn't list: the same 403, explained as that.
	resetAPIFlags(t)
	strangerCert, strangerKey, _ := writeOperatorCert(t)
	_, _, err = runAPI(append(noCert, "--client-cert", strangerCert, "--client-key", strangerKey)...)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || !strings.Contains(err.Error(), "the web app doesn't list --client-cert") {
		t.Errorf("an unlisted cert: err = %v, want the 403 explained", err)
	}
}

// The remediation is the CLI's, added only to the cert check's 403: the
// library error states the refusal, and another 403 (the operator-peer
// filter's) or error passes through unchanged (#397 review).
func TestExplainCertRefusal(t *testing.T) {
	refusal := &apiclient.APIError{Method: "GET", Path: "/status", StatusCode: http.StatusForbidden, Message: webtls.NotOperatorMessage}
	if err := explainCertRefusal(refusal, true); !strings.Contains(err.Error(), "OPERATOR_CERTS_PATH") || strings.Contains(err.Error(), "no client cert was presented") {
		t.Errorf("presented: %v", err)
	}
	if err := explainCertRefusal(refusal, false); !strings.Contains(err.Error(), "--client-cert and --client-key") {
		t.Errorf("not presented: %v", err)
	}
	if strings.Contains(refusal.Error(), "OPERATOR_CERTS_PATH") || strings.Contains(refusal.Error(), "--client-cert") {
		t.Errorf("the library error %q names a setting or flag", refusal)
	}
	for _, other := range []error{
		&apiclient.APIError{Method: "GET", Path: "/status", StatusCode: http.StatusForbidden, Message: "forbidden: not an operator peer"},
		errors.New("connection reset by peer"),
		nil,
	} {
		if got := explainCertRefusal(other, true); got != other { //nolint:errorlint // identity: passed through unwrapped
			t.Errorf("%v: got %v, want it unchanged", other, got)
		}
	}
}

// A regular file (stderr redirected to a log, say) is no terminal, so
// apiCanPrompt doesn't ask where no one would see it.
func TestIsTerminalRefusesARegularFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "err.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test
	if isTerminal(f) {
		t.Error("a regular file counted as a terminal")
	}
}

// reuseFirst hands the probe's connection to the first dial of its address
// only, and dials afresh after it and for any other address.
func TestReuseFirst(t *testing.T) {
	probe, _ := net.Pipe()
	var dialed []string
	dial := reuseFirst(probe, "api:80", func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		c, _ := net.Pipe()
		return c, nil
	})
	ctx := context.Background()
	if c, _ := dial(ctx, "tcp", "other:80"); c == probe {
		t.Error("another address got the probe's connection")
	}
	if c, _ := dial(ctx, "tcp", "api:80"); c != probe {
		t.Error("the first dial of the probed address didn't reuse its connection")
	}
	if c, _ := dial(ctx, "tcp", "api:80"); c == probe {
		t.Error("the probe's connection was handed out twice")
	}
	if strings.Join(dialed, " ") != "other:80 api:80" {
		t.Errorf("dialed %v, want other:80 then api:80", dialed)
	}
}

// statusArgs is `api status` against app, as its operator peer with its
// operator cert, plus extra flags.
func (app testWebApp) statusArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{"api", "status",
		"--private-key-file", writeKeyFile(t, app.cliPriv),
		"--local-addr", app.cliAddr.String(),
		"--server-public-key", app.pub.Base64(),
		"--endpoint", "127.0.0.1:" + strconv.Itoa(app.port),
		"--connect-timeout", "20s",
		"--client-cert", app.certFile,
		"--client-key", app.keyFile,
	}, extra...)
}

// On a terminal, the first connection shows the web app's fingerprint and
// asks; yes records it in --pins-file, and the next run trusts it unasked.
// Without a terminal and with no pin, the run is refused instead.
func TestAPIPinsTheServerOnFirstUse(t *testing.T) {
	resetAPIFlags(t)
	t.Cleanup(func() { resetAPIFlags(t) })
	app := startTestWebAppTunnel(t)
	pins := filepath.Join(t.TempDir(), "api-pins")

	origIn, origOut, origCan := apiPromptIn, apiPromptOut, apiCanPrompt
	t.Cleanup(func() { apiPromptIn, apiPromptOut, apiCanPrompt = origIn, origOut, origCan })
	var prompt bytes.Buffer
	apiPromptOut = &prompt

	apiCanPrompt = func() bool { return false }
	if _, _, err := runAPI(app.statusArgs(t, "--pins-file", pins)...); err == nil || !strings.Contains(err.Error(), "--server-fingerprint "+app.pin) {
		t.Errorf("no terminal, no pin: err = %v, want a refusal naming the fingerprint", err)
	}

	resetAPIFlags(t)
	apiCanPrompt = func() bool { return true }
	apiPromptIn = strings.NewReader("y\n")
	if _, _, err := runAPI(app.statusArgs(t, "--pins-file", pins)...); err != nil {
		t.Fatalf("first use, confirmed: %v", err)
	}
	if !strings.Contains(prompt.String(), app.pin) {
		t.Errorf("the prompt %q doesn't show the fingerprint", prompt.String())
	}
	if got, ok, err := apiclient.LookupPin(pins, app.pub.Base64()); err != nil || !ok || got != app.pin {
		t.Errorf("recorded pin = %q, %v, %v; want %q", got, ok, err, app.pin)
	}

	resetAPIFlags(t)
	prompt.Reset()
	apiPromptIn = strings.NewReader("")
	if _, _, err := runAPI(app.statusArgs(t, "--pins-file", pins)...); err != nil || prompt.Len() != 0 {
		t.Errorf("second use: err %v, prompt %q; want the recorded pin, unasked", err, prompt.String())
	}
}
