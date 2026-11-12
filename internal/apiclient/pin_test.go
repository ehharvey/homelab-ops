package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/webtls"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// tlsServer is a web app stand-in serving GET /status over TLS with a key
// derived from a fresh identity, admitting only operator's cert
// (webtls.RequireOperator); GET /drop closes the connection unanswered. It
// returns a dial that reaches it, and its pin.
func tlsServer(t *testing.T, operator *x509.Certificate) (DialFunc, string) {
	t.Helper()
	priv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := webtls.DeriveKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := webtls.SelfSigned(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(webtls.RequireOperator([]*x509.Certificate{operator}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/drop" {
			// A connection lost mid-request, after the handshake: a
			// restart, or the tunnel dropping.
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, `{"synced":true}`)
	})))
	srv.TLS = webtls.ServerConfig(c)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	var d net.Dialer
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	pin, err := webtls.PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return dial, pin
}

func operatorPair(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	p, err := cert.Generate(cert.Options{CommonName: "operator", ValidityDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	tc, err := tls.X509KeyPair(p.CertPEM, p.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(tc.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return tc, leaf
}

func mustLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	_, leaf := operatorPair(t)
	return leaf
}

func TestPinnedHTTPClientPresentsTheCertAndChecksThePin(t *testing.T) {
	opCert, opLeaf := operatorPair(t)
	otherCert, _ := operatorPair(t)
	dial, pin := tlsServer(t, opLeaf)
	_, otherPin := tlsServer(t, opLeaf)
	ctx := context.Background()

	client := func(o TLSOptions) *http.Client {
		hc, err := PinnedHTTPClient(dial, o)
		if err != nil {
			t.Fatalf("PinnedHTTPClient: %v", err)
		}
		return hc
	}
	// The base URL names an address nothing listens on: only dial reaches
	// the server.
	status := func(o TLSOptions) error {
		c, err := New("https://10.100.0.1", client(o))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Status(ctx)
		return err
	}
	if err := status(TLSOptions{ClientCert: &opCert, ServerPin: pin}); err != nil {
		t.Errorf("operator cert, right pin: %v", err)
	}

	err := status(TLSOptions{ClientCert: &opCert, ServerPin: otherPin, PinSource: "--server-fingerprint"})
	var mismatch *PinMismatchError
	if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), "pass the new one as --server-fingerprint") {
		t.Errorf("a wrong pin: err = %v, want a PinMismatchError naming its source as the fix", err)
	}

	// The web app's refusal of a cert is an HTTP answer, so it can't be
	// mistaken for a dropped connection, nor one for it (#397).
	for name, o := range map[string]TLSOptions{
		"no client cert":   {ServerPin: pin},
		"an unlisted cert": {ClientCert: &otherCert, ServerPin: pin},
	} {
		var apiErr *APIError
		if err := status(o); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Message != webtls.NotOperatorMessage {
			t.Errorf("%s: err = %v, want a 403 APIError saying %q", name, err, webtls.NotOperatorMessage)
		}
	}
	resp, err := client(TLSOptions{ClientCert: &opCert, ServerPin: pin}).Get("https://10.100.0.1/drop")
	if err == nil {
		resp.Body.Close() //nolint:errcheck,gosec // a test
		t.Fatal("GET /drop answered, want the dropped connection's error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) || errors.As(err, &mismatch) {
		t.Errorf("a connection dropped after the handshake: err = %v, want a plain transport error", err)
	}

	for _, bad := range []string{"", "sha256//nope"} {
		if _, err := PinnedHTTPClient(dial, TLSOptions{ServerPin: bad}); err == nil {
			t.Errorf("PinnedHTTPClient with pin %q succeeded", bad)
		}
	}
}

func TestLoadClientCert(t *testing.T) {
	if c, err := LoadClientCert("", ""); c != nil || err != nil {
		t.Errorf("neither: %v, %v; want no cert", c, err)
	}
	for _, half := range [][2]string{{"c.crt", ""}, {"", "c.key"}} {
		if _, err := LoadClientCert(half[0], half[1]); err == nil || !strings.Contains(err.Error(), "go together") {
			t.Errorf("only %q: err = %v", half, err)
		}
	}
	p, err := cert.Generate(cert.Options{CommonName: "operator", ValidityDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key")
	if err := os.WriteFile(crt, p.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, p.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadClientCert(crt, key); err != nil || c == nil {
		t.Errorf("both: %v, %v", c, err)
	}
}

func TestServerFingerprintReadsTheKeyWithoutACert(t *testing.T) {
	_, opLeaf := operatorPair(t)
	dial, pin := tlsServer(t, opLeaf)
	got, err := ServerFingerprint(context.Background(), dial, "ignored:443")
	if err != nil || got != pin {
		t.Errorf("ServerFingerprint = %q, %v; want %q", got, err, pin)
	}
}

// pinning is a Pinning against a fixed probe answer, recording in a temp
// file; *probes counts the probes.
func pinning(t *testing.T, got string) (p Pinning, file string, probes *int) {
	t.Helper()
	file = filepath.Join(t.TempDir(), "conf", "api-pins")
	probes = new(int)
	return Pinning{
		File:   file,
		Server: "server-key=",
		Probe:  func(context.Context) (string, error) { *probes++; return got, nil },
	}, file, probes
}

type confirmFunc = func(ctx context.Context, fp, previous string) (bool, error)

func confirmWith(answer string) (confirmFunc, *bytes.Buffer) {
	var out bytes.Buffer
	return PromptConfirm(strings.NewReader(answer), &out, "the pins file"), &out
}

// First use: the fingerprint is shown and confirmed y/n; yes records it,
// and the next run uses it without asking or probing (#397's Done-when).
func TestPinningFirstUseConfirmsAndRecords(t *testing.T) {
	_, fpA := tlsServer(t, mustLeaf(t))
	p, file, probes := pinning(t, fpA)

	var asked bool
	confirm, out := confirmWith("y\n")
	p.Confirm = func(ctx context.Context, fp, prev string) (bool, error) { asked = true; return confirm(ctx, fp, prev) }
	pin, source, err := p.Resolve(context.Background())
	if err != nil || pin != fpA || !asked {
		t.Fatalf("first use, yes: pin %q, asked %v, err %v; want %q", pin, asked, err, fpA)
	}
	if !strings.Contains(out.String(), fpA) || !strings.Contains(out.String(), "[y/N]") || !strings.Contains(out.String(), "first connection") {
		t.Errorf("the prompt %q doesn't show the fingerprint and ask y/n", out)
	}
	if !strings.Contains(source, file) {
		t.Errorf("source = %q, want it to name %s", source, file)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("pins file: %v, %v; want it written 0600", info, err)
	}

	asked, *probes = false, 0
	if pin, _, err := p.Resolve(context.Background()); err != nil || pin != fpA || asked || *probes != 0 {
		t.Errorf("second use: pin %q, asked %v, %d probes, err %v; want the recorded pin, unasked and unprobed", pin, asked, *probes, err)
	}
}

func TestPinningFirstUseRefusedRecordsNothing(t *testing.T) {
	p, file, _ := pinning(t, "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw=")
	for _, answer := range []string{"n\n", "\n", "yes please\n", ""} {
		p.Confirm, _ = confirmWith(answer)
		if _, _, err := p.Resolve(context.Background()); err == nil {
			t.Errorf("answer %q: trusted", answer)
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("pins file after refusals: %v, want none", err)
	}
}

// A changed server key is refused against the recorded pin, by the real
// request's handshake, and nothing asks; the refusal says how to re-pin.
func TestPinningRefusesAChangedKey(t *testing.T) {
	opCert, opLeaf := operatorPair(t)
	_, fpA := tlsServer(t, opLeaf)
	dialB, fpB := tlsServer(t, opLeaf)
	p, file, probes := pinning(t, fpB)
	if err := RecordPin(file, p.Server, fpA); err != nil {
		t.Fatal(err)
	}
	p.Confirm = func(context.Context, string, string) (bool, error) {
		t.Error("asked to confirm a changed key")
		return true, nil
	}
	pin, source, err := p.Resolve(context.Background())
	if err != nil || pin != fpA || *probes != 0 {
		t.Fatalf("Resolve = %q, %v after %d probes; want the recorded pin, unprobed", pin, err, *probes)
	}
	hc, err := PinnedHTTPClient(dialB, TLSOptions{ClientCert: &opCert, ServerPin: pin, PinSource: source})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("https://10.100.0.1", hc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Status(context.Background())
	var mismatch *PinMismatchError
	if !errors.As(err, &mismatch) || mismatch.Got != fpB || mismatch.Want != fpA || !strings.Contains(err.Error(), "--repin") {
		t.Errorf("changed key: err = %v, want a PinMismatchError from %s to %s that names --repin", err, fpA, fpB)
	}
}

// --repin asks afresh and replaces the recorded pin, keeping every other
// line; with no terminal it refuses, and with an unchanged key it asks
// nothing.
func TestPinningRepinReplacesTheRecordedPin(t *testing.T) {
	_, fpA := tlsServer(t, mustLeaf(t))
	_, fpB := tlsServer(t, mustLeaf(t))
	p, file, _ := pinning(t, fpB)
	p.Repin = true
	if err := RecordPin(file, "other=", fpA); err != nil {
		t.Fatal(err)
	}
	if err := RecordPin(file, p.Server, fpA); err != nil {
		t.Fatal(err)
	}

	if _, _, err := p.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "--repin needs a terminal") {
		t.Errorf("--repin, no terminal: err = %v", err)
	}

	confirm, out := confirmWith("y\n")
	p.Confirm = confirm
	if pin, _, err := p.Resolve(context.Background()); err != nil || pin != fpB {
		t.Fatalf("--repin, yes: %q, %v; want %q", pin, err, fpB)
	}
	if !strings.Contains(out.String(), "replace the recorded "+fpA) {
		t.Errorf("the prompt %q doesn't name the pin it replaces", out)
	}
	for server, want := range map[string]string{p.Server: fpB, "other=": fpA} {
		if got, ok, err := LookupPin(file, server); err != nil || !ok || got != want {
			t.Errorf("after --repin, %s: %q, %v, %v; want %q", server, got, ok, err, want)
		}
	}

	p.Confirm = func(context.Context, string, string) (bool, error) {
		t.Error("--repin asked about an unchanged key")
		return false, nil
	}
	if pin, _, err := p.Resolve(context.Background()); err != nil || pin != fpB {
		t.Errorf("--repin, unchanged key: %q, %v", pin, err)
	}
}

// Non-interactive: --server-fingerprint is the only pin, used without a
// probe and never recorded; with neither it nor a recorded pin the run
// refuses rather than trusting the key.
func TestPinningNonInteractive(t *testing.T) {
	_, fpA := tlsServer(t, mustLeaf(t))
	p, file, probes := pinning(t, fpA)
	p.Expected = fpA
	if pin, source, err := p.Resolve(context.Background()); err != nil || pin != fpA || source != "--server-fingerprint" || *probes != 0 {
		t.Errorf("expected: %q from %q, %v, %d probes", pin, source, err, *probes)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("an explicit pin was recorded: %v", err)
	}

	p.Expected = ""
	if _, _, err := p.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "--server-fingerprint "+fpA) {
		t.Errorf("no pin, no terminal: err = %v, want a refusal naming the flag and the key", err)
	}
}

// Ctrl-C at the y/N prompt: the cancelled context ends the wait, though
// nothing is ever typed.
func TestPromptConfirmStopsOnCancel(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { w.Close() }) //nolint:errcheck,gosec // a test
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		_, err := PromptConfirm(in, io.Discard, "the pins file")(ctx, "sha256//x", "")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt ignored the cancelled context")
	}
}

// A pins file edited by hand often lacks its last newline; recording
// another pin must not glue the two lines together.
func TestRecordPinAfterAMissingNewline(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pins")
	fpA := "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
	_, fpB := tlsServer(t, mustLeaf(t))
	if err := os.WriteFile(file, []byte("# mine\nA= "+fpA), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecordPin(file, "B=", fpB); err != nil {
		t.Fatal(err)
	}
	for server, want := range map[string]string{"A=": fpA, "B=": fpB} {
		if got, ok, err := LookupPin(file, server); err != nil || !ok || got != want {
			t.Errorf("%s: %q, %v, %v; want %q", server, got, ok, err, want)
		}
	}
	b, err := os.ReadFile(file)
	if err != nil || !strings.HasPrefix(string(b), "# mine\n") || !strings.HasSuffix(string(b), "\n") {
		t.Errorf("pins file %q, %v: want the comment kept and a final newline", b, err)
	}
}

func TestLookupPinReadsTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pins")
	fp := "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
	if err := os.WriteFile(file, []byte("# pins\nother= "+fp+"\nme= "+fp+"  # laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := LookupPin(file, "me="); err != nil || !ok || got != fp {
		t.Errorf("LookupPin = %q, %v, %v", got, ok, err)
	}
	if _, ok, err := LookupPin(file, "nobody="); err != nil || ok {
		t.Errorf("an unknown server: %v, %v", ok, err)
	}
	if err := os.WriteFile(file, []byte("me= not-a-pin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LookupPin(file, "me="); !errors.Is(err, ErrMalformedPin) {
		t.Errorf("a malformed pin: err = %v, want ErrMalformedPin", err)
	}
}

// A malformed line for another server is none of this lookup's business: it
// must not block every web app (#397 review).
func TestLookupPinSkipsOtherServersMalformedLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pins")
	fp := "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
	if err := os.WriteFile(file, []byte("oldserver\nthird= a b\nfourth= not-a-pin\nme= "+fp+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := LookupPin(file, "me="); err != nil || !ok || got != fp {
		t.Errorf("me=: %q, %v, %v; want %q", got, ok, err, fp)
	}
	if _, ok, err := LookupPin(file, "nobody="); err != nil || ok {
		t.Errorf("an unknown server: %v, %v; want not found, no error", ok, err)
	}
	for _, line := range []string{"me=", "me= " + fp + " extra", "me= not-a-pin"} {
		if err := os.WriteFile(file, []byte("other= "+fp+"\n"+line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LookupPin(file, "me="); !errors.Is(err, ErrMalformedPin) || !strings.Contains(err.Error(), ":2:") {
			t.Errorf("own line %q: err = %v, want ErrMalformedPin naming line 2", line, err)
		}
	}
}

// --repin recovers from a malformed line for this server without a hand
// edit: it asks, replaces that line, and keeps every other server's line,
// malformed or not.
func TestPinningRepinReplacesAMalformedLine(t *testing.T) {
	_, fp := tlsServer(t, mustLeaf(t))
	p, file, _ := pinning(t, fp)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("oldserver\n"+p.Server+" not-a-pin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Resolve(context.Background()); !errors.Is(err, ErrMalformedPin) {
		t.Errorf("without --repin: err = %v, want ErrMalformedPin", err)
	}

	p.Repin = true
	p.Confirm, _ = confirmWith("y\n")
	if pin, _, err := p.Resolve(context.Background()); err != nil || pin != fp {
		t.Fatalf("--repin, yes: %q, %v; want %q", pin, err, fp)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := "oldserver\n" + p.Server + " " + fp + "\n"; string(b) != want {
		t.Errorf("pins file after --repin = %q, want %q", b, want)
	}
}

// A symlinked pins file (a dotfiles checkout, say) stays a symlink: the
// rename replaces its target, which is created if the link dangles.
func TestRecordPinFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	fp := "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
	target := filepath.Join(dir, "dotfiles", "api-pins")
	link := filepath.Join(dir, "conf", "api-pins")
	for _, d := range []string{filepath.Dir(target), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../dotfiles/api-pins", link); err != nil {
		t.Fatal(err)
	}
	for _, server := range []string{"A=", "B="} {
		if err := RecordPin(link, server, fp); err != nil {
			t.Fatalf("RecordPin %s through a symlink: %v", server, err)
		}
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the pins file is no longer a symlink: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "A= "+fp+"\nB= "+fp+"\n" {
		t.Errorf("the link's target = %q, %v; want both pins", b, err)
	}
}

// Runs recording pins at once each land: the read-modify-write is locked.
func TestRecordPinConcurrentRunsKeepEveryPin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "conf", "api-pins")
	fp := "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
	const runs = 16
	errs := make(chan error, runs)
	for i := range runs {
		go func() { errs <- RecordPin(file, fmt.Sprintf("server-%d=", i), fp) }()
	}
	for range runs {
		if err := <-errs; err != nil {
			t.Fatalf("RecordPin: %v", err)
		}
	}
	for i := range runs {
		if _, ok, err := LookupPin(file, fmt.Sprintf("server-%d=", i)); err != nil || !ok {
			t.Errorf("server-%d=: recorded %v, %v; want every concurrent pin kept", i, ok, err)
		}
	}
}
