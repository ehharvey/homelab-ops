package webtls

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// identity is a literal WireGuard private key: 32 bytes of b.
func identity(b byte) wireguard.PrivateKey {
	var k wireguard.PrivateKey
	for i := range k {
		k[i] = b
	}
	return k
}

func fingerprint(t *testing.T, id wireguard.PrivateKey) string {
	t.Helper()
	key, err := DeriveKey(id)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	fp, err := PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// The same identity gives the same key, so the pin survives a restart; a
// different identity gives a different one (#397's Done-when).
func TestDeriveKeyIsStablePerIdentity(t *testing.T) {
	a1, a2, b := fingerprint(t, identity(1)), fingerprint(t, identity(1)), fingerprint(t, identity(2))
	if a1 != a2 {
		t.Errorf("the same identity derived %s, then %s", a1, a2)
	}
	if a1 == b {
		t.Errorf("two identities derived the same key %s", a1)
	}
}

// The derivation is pinned: every client's recorded pin depends on it, so a
// change to it must come with a new keyInfo version, on purpose, and this
// value updated with it. The value was cross-checked outside Go: HKDF-SHA256
// with Python's hmac, and the SPKI from openssl ec on the first candidate.
func TestDeriveKeyIsPinned(t *testing.T) {
	const want = "sha256//1cTo/jJRHejfZmjBQ03eeJaF3ko2t/JFbZ6Nbg+TMqw="
	if got := fingerprint(t, identity(1)); got != want {
		t.Errorf("identity 0x01... derives %s, want %s: did the derivation change without a new keyInfo version?", got, want)
	}
}

func TestDeriveKeyRefusesAZeroIdentity(t *testing.T) {
	if _, err := DeriveKey(wireguard.PrivateKey{}); err == nil {
		t.Error("DeriveKey of an all-zero identity succeeded")
	}
}

// A cert re-made at each start carries the same public key, so its pin, the
// SPKI's, is the same, though the certs differ.
func TestSelfSignedKeepsTheKeysFingerprint(t *testing.T) {
	key, err := DeriveKey(identity(3))
	if err != nil {
		t.Fatal(err)
	}
	c1, err := SelfSigned(key, time.Now())
	if err != nil {
		t.Fatalf("SelfSigned: %v", err)
	}
	c2, err := SelfSigned(key, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("SelfSigned: %v", err)
	}
	if bytes.Equal(c1.Certificate[0], c2.Certificate[0]) {
		t.Error("two starts made the same cert, want a fresh one each")
	}
	want := fingerprint(t, identity(3))
	for i, c := range []tls.Certificate{c1, c2} {
		if got := Fingerprint(c.Leaf.RawSubjectPublicKeyInfo); got != want {
			t.Errorf("cert %d's SPKI fingerprint = %s, want the key's %s", i+1, got, want)
		}
	}
}

func TestParseFingerprint(t *testing.T) {
	good := fingerprint(t, identity(4))
	if got, err := ParseFingerprint(" " + good + "\n"); err != nil || got != good {
		t.Errorf("ParseFingerprint(%q) = %q, %v", good, got, err)
	}
	for _, bad := range []string{"", strings.TrimPrefix(good, "sha256//"), "sha1//" + strings.TrimPrefix(good, "sha256//"), "sha256//not-base64!", "sha256//AAAA"} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Errorf("ParseFingerprint(%q) accepted it", bad)
		}
	}
}

func TestParseCertsPEM(t *testing.T) {
	a, b := genCert(t), genCert(t)
	certs, err := ParseCertsPEM(append(append([]byte("# laptop\n"), a.CertPEM...), b.CertPEM...))
	if err != nil || len(certs) != 2 {
		t.Fatalf("two certs: %d, %v", len(certs), err)
	}
	if _, err := ParseCertsPEM([]byte("# nothing here\n")); err == nil {
		t.Error("a file with no cert parsed")
	}
	if _, err := ParseCertsPEM(append(a.CertPEM, a.KeyPEM...)); err == nil || !strings.Contains(err.Error(), "never a private key") {
		t.Errorf("a cert with its private key: err = %v, want it refused", err)
	}
	bogus := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")})
	if _, err := ParseCertsPEM(bogus); err == nil {
		t.Error("a CERTIFICATE block that isn't one parsed")
	}
}

func genCert(t *testing.T) *cert.Pair {
	t.Helper()
	p, err := cert.Generate(cert.Options{CommonName: "operator", ValidityDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// handshake runs ServerConfig's server against a client presenting client
// (nil: none) over a pipe, and returns the server's handshake error and the
// certs the client presented.
func handshake(t *testing.T, srv *tls.Config, client *tls.Certificate) ([]*x509.Certificate, error) {
	t.Helper()
	sc, cc := net.Pipe()
	defer sc.Close()                                                            //nolint:errcheck // test
	defer cc.Close()                                                            //nolint:errcheck // test
	ccfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13} //nolint:gosec // G402: the test checks the server side only
	if client != nil {
		ccfg.Certificates = []tls.Certificate{*client}
	}
	go func() {
		c := tls.Client(cc, ccfg)
		if c.Handshake() == nil {
			// TLS 1.3: the server's verdict on the client cert arrives
			// after the client's handshake; read it.
			_, _ = c.Read(make([]byte, 1))
		}
		_ = c.Close()
	}()
	s := tls.Server(sc, srv)
	err := s.Handshake()
	return s.ConnectionState().PeerCertificates, err
}

func testServerConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := DeriveKey(identity(5))
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := SelfSigned(key, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig(serverCert)
}

func keyPair(t *testing.T, p *cert.Pair) *tls.Certificate {
	t.Helper()
	c, err := tls.X509KeyPair(p.CertPEM, p.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

// The handshake asks for a cert but refuses none: whether it is an
// operator's is RequireOperator's answer, over HTTP (#397). The cert it
// passes on is the one presented.
func TestServerConfigAsksForAClientCertWithoutRequiringOne(t *testing.T) {
	srv := testServerConfig(t)
	presented := genCert(t)
	got, err := handshake(t, srv, keyPair(t, presented))
	if err != nil {
		t.Fatalf("a self-signed client cert: handshake = %v, want it to finish", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Raw, mustParse(t, presented.CertPEM).Raw) {
		t.Errorf("the server saw %d client certs, want exactly the one presented", len(got))
	}
	if got, err := handshake(t, srv, nil); err != nil || len(got) != 0 {
		t.Errorf("no client cert: handshake = %v with %d certs, want it to finish with none", err, len(got))
	}
}

// Presenting a listed cert isn't enough: the handshake still makes the client
// prove it holds that cert's key, so a copied public cert gets nobody in.
func TestServerConfigRequiresTheClientCertsKey(t *testing.T) {
	srv := testServerConfig(t)
	listed := keyPair(t, genCert(t))
	other := keyPair(t, genCert(t))
	stolen := &tls.Certificate{Certificate: listed.Certificate, PrivateKey: other.PrivateKey}
	if _, err := handshake(t, srv, stolen); err == nil {
		t.Error("a listed cert signed for with another key: handshake finished, want it refused")
	}
}

func TestRequireOperatorAdmitsOnlyListedCerts(t *testing.T) {
	listed, unlisted := genCert(t), genCert(t)
	h := RequireOperator([]*x509.Certificate{mustParse(t, genCert(t).CertPEM), mustParse(t, listed.CertPEM)},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "api "+r.URL.Path) }))
	serve := func(state *tls.ConnectionState) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "https://10.100.0.1/status", nil)
		r.TLS = state
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	presenting := func(p *cert.Pair) *tls.ConnectionState {
		return &tls.ConnectionState{PeerCertificates: []*x509.Certificate{mustParse(t, p.CertPEM)}}
	}

	if code, body := serve(presenting(listed)); code != http.StatusOK || body != "api /status" {
		t.Errorf("listed operator cert = %d %q, want 200 from the handler", code, body)
	}
	for name, state := range map[string]*tls.ConnectionState{
		"an unlisted cert": presenting(unlisted),
		"no client cert":   {},
		"not over TLS":     nil,
	} {
		if code, body := serve(state); code != http.StatusForbidden || body != NotOperatorMessage {
			t.Errorf("%s = %d %q, want 403 %q", name, code, body, NotOperatorMessage)
		}
	}
}

func mustParse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	certs, err := ParseCertsPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certs[0]
}
