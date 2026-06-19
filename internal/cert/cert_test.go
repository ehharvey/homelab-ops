package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testOptions() Options {
	return Options{CommonName: "bootstrap@test-host", ValidityDays: 3650}
}

func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("failed to decode cert PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return c
}

func parseKey(t *testing.T, keyPEM []byte) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		t.Fatalf("failed to decode key PEM block")
	}
	k, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse EC private key: %v", err)
	}
	return k
}

func TestGenerateRoundTripsAsPEM(t *testing.T) {
	pair, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	parseCert(t, pair.CertPEM)
	parseKey(t, pair.KeyPEM)
}

func TestGenerateIsSelfSigned(t *testing.T) {
	pair, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := parseCert(t, pair.CertPEM)
	// CheckSignatureFrom enforces CA-signing semantics (requires IsCA), which
	// a client-auth leaf cert intentionally doesn't have, so verify the
	// self-signature directly instead.
	if err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature); err != nil {
		t.Fatalf("certificate is not self-signed: %v", err)
	}
	if c.Issuer.CommonName != c.Subject.CommonName {
		t.Errorf("Issuer %q != Subject %q, want equal for a self-signed cert", c.Issuer.CommonName, c.Subject.CommonName)
	}
}

func TestGenerateKeyUsage(t *testing.T) {
	pair, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := parseCert(t, pair.CertPEM)

	wantUsage := x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature
	if c.KeyUsage != wantUsage {
		t.Errorf("KeyUsage = %v, want %v", c.KeyUsage, wantUsage)
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [ClientAuth]", c.ExtKeyUsage)
	}
}

func TestGenerateAlgorithm(t *testing.T) {
	pair, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := parseCert(t, pair.CertPEM)

	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("public key type = %T, want *ecdsa.PublicKey", c.PublicKey)
	}
	if pub.Curve != elliptic.P384() {
		t.Errorf("curve = %v, want P384", pub.Curve.Params().Name)
	}
}

func TestGenerateValidityWindow(t *testing.T) {
	opts := testOptions()
	opts.ValidityDays = 30
	pair, err := Generate(opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := parseCert(t, pair.CertPEM)

	if time.Since(c.NotBefore) > time.Minute {
		t.Errorf("NotBefore = %v, want close to now", c.NotBefore)
	}
	gotDuration := c.NotAfter.Sub(c.NotBefore)
	wantDuration := 30 * 24 * time.Hour
	delta := gotDuration - wantDuration
	if delta < 0 {
		delta = -delta
	}
	if delta > 24*time.Hour {
		t.Errorf("validity window = %v, want ~%v", gotDuration, wantDuration)
	}
}

func TestGenerateCommonName(t *testing.T) {
	pair, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := parseCert(t, pair.CertPEM)
	if c.Subject.CommonName != "bootstrap@test-host" {
		t.Errorf("CommonName = %q, want %q", c.Subject.CommonName, "bootstrap@test-host")
	}
}

func TestGenerateProducesUniquePairs(t *testing.T) {
	a, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	certA, certB := parseCert(t, a.CertPEM), parseCert(t, b.CertPEM)
	if certA.SerialNumber.Cmp(certB.SerialNumber) == 0 {
		t.Errorf("expected distinct serial numbers across calls")
	}
	if string(a.KeyPEM) == string(b.KeyPEM) {
		t.Errorf("expected distinct key material across calls")
	}
}

func TestWriteCreatesFilesWithExpectedPermissions(t *testing.T) {
	dir := t.TempDir()
	pair, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	certPath, keyPath, err := Write(dir, pair, false)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if certPath != filepath.Join(dir, "client.crt") {
		t.Errorf("certPath = %q, want %q", certPath, filepath.Join(dir, "client.crt"))
	}
	if keyPath != filepath.Join(dir, "client.key") {
		t.Errorf("keyPath = %q, want %q", keyPath, filepath.Join(dir, "client.key"))
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("key permissions = %o, want 0600", perm)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert: %v", err)
	}
	if perm := certInfo.Mode().Perm(); perm != 0o644 {
		t.Errorf("cert permissions = %o, want 0644", perm)
	}

	gotCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if string(gotCert) != string(pair.CertPEM) {
		t.Errorf("written cert content does not match input")
	}

	gotKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if string(gotKey) != string(pair.KeyPEM) {
		t.Errorf("written key content does not match input")
	}
}

func TestWriteRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	first, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := Write(dir, first, false); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	second, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := Write(dir, second, false); err == nil {
		t.Fatalf("expected error overwriting without force, got nil")
	}

	gotCert, err := os.ReadFile(filepath.Join(dir, "client.crt"))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if string(gotCert) != string(first.CertPEM) {
		t.Errorf("original cert was overwritten despite missing --force")
	}
}

func TestWriteOverwritesWithForce(t *testing.T) {
	dir := t.TempDir()
	first, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := Write(dir, first, false); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	second, err := Generate(testOptions())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := Write(dir, second, true); err != nil {
		t.Fatalf("forced Write: %v", err)
	}

	gotCert, err := os.ReadFile(filepath.Join(dir, "client.crt"))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if string(gotCert) != string(second.CertPEM) {
		t.Errorf("expected overwritten cert content, got original")
	}
}
