// Package secureboot is the project's own UEFI Secure Boot key hierarchy and
// the signing that uses it (docs/Decisions.md §31, Secure Boot; refines §6).
//
// It generates the three key pairs firmware knows about, builds the payloads
// a machine in Setup Mode enrolls on first boot, and Authenticode-signs
// PE/EFI binaries (a UKI) with the db key:
//
//   - PK (Platform Key) is a self-signed root. It signs KEK updates.
//   - KEK (Key Exchange Key) is issued by PK. It signs db updates.
//   - db (signature database) is issued by KEK. Firmware runs an image whose
//     Authenticode signature chains to a db certificate.
//
// The X.509 chain PK -> KEK -> db is not something firmware checks (it trusts
// each variable's certificates directly, as partial chains), but it lets
// `openssl verify` check the hierarchy, and it is the shape Microsoft's own
// db and KEK certificates have (each issued by a root that is not enrolled).
//
// Keys are RSA-2048 and signatures SHA-256: RSA-2048 is the one size every
// firmware is expected to handle, and both Microsoft UEFI CA certificates
// enrolled beside ours (internal/third_party/msuefica) are RSA-2048 with
// sha256WithRSAEncryption.
//
// Everything here is stdlib crypto plus golang.org/x/crypto/cryptobyte for
// DER building (already in the module graph); the PKCS#7 SignedData both
// authenticated variables and Authenticode need is built in pkcs7.go rather
// than imported, per the internal/cert precedent.
package secureboot

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// RSABits is the size of every generated key.
const RSABits = 2048

// Organization is the Subject Organization of every generated certificate.
const Organization = "homelab-ops"

// KeyPair is one level of the hierarchy: a certificate and the key that
// signs with it. Key is a crypto.Signer so a later hardware token (§31's
// YubiKey for PK/KEK) can stand in for an in-memory key.
type KeyPair struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// Hierarchy is a generated PK/KEK/db set and the SignatureOwner GUID its
// signature-list entries carry.
type Hierarchy struct {
	OwnerGUID GUID
	PK        KeyPair
	KEK       KeyPair
	DB        KeyPair
}

// Options configures Generate.
type Options struct {
	// ValidityDays is how long each certificate is valid for. Firmware does
	// not check validity periods, but openssl and sbverify report them.
	ValidityDays int
	// Now is the certificates' NotBefore. The zero value means time.Now().
	Now time.Time
}

// Generate creates a new PK/KEK/db hierarchy and a random owner GUID. It
// performs no network I/O.
func Generate(opts Options) (*Hierarchy, error) {
	if opts.ValidityDays <= 0 {
		return nil, fmt.Errorf("validity days must be positive, got %d", opts.ValidityDays)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	notAfter := now.AddDate(0, 0, opts.ValidityDays)

	owner, err := NewGUID()
	if err != nil {
		return nil, err
	}

	pk, err := newKeyPair(&x509.Certificate{
		Subject:               pkix.Name{Organization: []string{Organization}, CommonName: Organization + " Platform Key"},
		NotBefore:             now,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("PK: %w", err)
	}

	kek, err := newKeyPair(&x509.Certificate{
		Subject:               pkix.Name{Organization: []string{Organization}, CommonName: Organization + " Key Exchange Key"},
		NotBefore:             now,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}, pk)
	if err != nil {
		return nil, fmt.Errorf("KEK: %w", err)
	}

	db, err := newKeyPair(&x509.Certificate{
		Subject:               pkix.Name{Organization: []string{Organization}, CommonName: Organization + " Signature Database Key"},
		NotBefore:             now,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
	}, kek)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}

	return &Hierarchy{OwnerGUID: owner, PK: *pk, KEK: *kek, DB: *db}, nil
}

// newKeyPair generates an RSA key and a certificate from tmpl, issued by
// parent, or self-signed when parent is nil.
func newKeyPair(tmpl *x509.Certificate, parent *KeyPair) (*KeyPair, error) {
	key, err := rsa.GenerateKey(rand.Reader, RSABits)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	tmpl.SerialNumber = serial
	tmpl.SignatureAlgorithm = x509.SHA256WithRSA

	issuer, signer := tmpl, crypto.Signer(key)
	if parent != nil {
		issuer, signer = parent.Cert, parent.Key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer, &key.PublicKey, signer)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return &KeyPair{Cert: cert, Key: key}, nil
}

// CertPEM returns the pair's certificate, PEM-encoded.
func (kp KeyPair) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kp.Cert.Raw})
}

// KeyPEM returns the pair's private key as PEM-encoded PKCS#8, the form
// sbsign, efitools and openssl all read.
func (kp KeyPair) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(kp.Key)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// LoadKeyPair parses a PEM certificate and a PEM private key (PKCS#8 or
// PKCS#1) and checks that they belong together.
func LoadKeyPair(certPEM, keyPEM []byte) (*KeyPair, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM CERTIFICATE block in certificate")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}

	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("no PEM block in private key")
	}
	var key *rsa.PrivateKey
	switch kb.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#8 key: %w", err)
		}
		k, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is %T, want RSA", parsed)
		}
		key = k
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#1 key: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported private key PEM type %q", kb.Type)
	}

	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("private key does not match certificate")
	}
	return &KeyPair{Cert: cert, Key: key}, nil
}
