// Package cert generates self-signed client certificates for direct Incus
// API authentication, entirely offline. It mirrors the shape of Incus's own
// client certs (ECDSA P-384, 10-year validity, client-auth extended key
// usage) so the result is interchangeable with what `incus` itself would
// generate.
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Options configures certificate generation.
type Options struct {
	// CommonName is the Subject CommonName of the generated certificate.
	CommonName string
	// ValidityDays is how long the certificate remains valid for.
	ValidityDays int
}

// Pair holds a PEM-encoded certificate and private key.
type Pair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Generate creates a self-signed ECDSA P-384 client certificate/key pair
// suitable for direct Incus client authentication. It performs no network
// I/O.
func Generate(opts Options) (*Pair, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.AddDate(0, 0, opts.ValidityDays)

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"homelab-ops"}, CommonName: opts.CommonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	return &Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Fingerprint computes certPEM's SHA-256 fingerprint over the raw DER
// certificate bytes, hex-encoded — the same value Incus's own `incus
// config trust` commands report, so it can be used directly with
// DELETE /1.0/certificates/<fingerprint>.
func Fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("decode cert PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	sum := sha256.Sum256(c.Raw)
	return fmt.Sprintf("%x", sum), nil
}

// Write persists the pair to <dir>/client.crt and <dir>/client.key. The key
// file is created with 0600 permissions. Write refuses to overwrite existing
// files unless force is true.
func Write(dir string, p *Pair, force bool) (certPath, keyPath string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", "", fmt.Errorf("create output dir: %w", err)
	}

	certPath = filepath.Join(dir, "client.crt")
	keyPath = filepath.Join(dir, "client.key")

	if !force {
		for _, path := range []string{certPath, keyPath} {
			if _, statErr := os.Stat(path); statErr == nil {
				return "", "", fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
		}
	}

	if err := os.WriteFile(certPath, p.CertPEM, 0o644); err != nil { //nolint:gosec // cert is not secret, 0644 is intentional
		return "", "", fmt.Errorf("write cert: %w", err)
	}

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // dir/filename are constructed from caller-supplied --output-dir, not untrusted external input
	if err != nil {
		return "", "", fmt.Errorf("open key file: %w", err)
	}
	defer func() {
		if closeErr := keyOut.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close key file: %w", closeErr)
		}
	}()

	if _, err := keyOut.Write(p.KeyPEM); err != nil {
		return "", "", fmt.Errorf("write key: %w", err)
	}

	return certPath, keyPath, err
}
