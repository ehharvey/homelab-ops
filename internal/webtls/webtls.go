// Package webtls is the web app API's TLS (#397, docs/Decisions.md §28, §32):
// the server's key and self-signed cert, the public-key fingerprint clients
// pin it by, the server config, and the middleware that admits only clients
// presenting a listed operator cert.
//
// Until step-ca (#506) issues the web app's cert, its TLS key is derived from
// its WireGuard identity (DeriveKey), so it adds no secret on disk and
// survives a restart, or a host move (#245), along with that identity. The
// cert around it is re-made at every start (SelfSigned), so clients pin the
// public key (Fingerprint), never the cert.
//
// Operator certs are pinned too, by their SHA-256, exactly as Incus trusts
// them (§4): there is no CA, and the web app holds only their public halves.
package webtls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// keyInfo is HKDF's info string for DeriveKey. Its version is part of the
// derivation: changing anything about how the key is made (the curve, the
// sampling) means a new version, and a new key every client must re-pin.
const keyInfo = "homelab-ops web app API TLS key, P-256, v1 (#397)"

// keyCandidates is how many 32-byte candidates DeriveKey draws from HKDF.
// A candidate is rejected only when it isn't a valid P-256 scalar (zero, or
// not below the group order), a chance of about 2^-32 each.
const keyCandidates = 8

// DeriveKey derives the web app's P-256 TLS key from its WireGuard private
// key: HKDF-SHA256 over the identity, with keyInfo as the info string, read
// as a scalar. The same identity always gives the same key, so the key's
// fingerprint survives restarts; a different identity gives a different one.
// It touches no store, file or setting.
func DeriveKey(identity wireguard.PrivateKey) (*ecdsa.PrivateKey, error) {
	if identity == (wireguard.PrivateKey{}) {
		return nil, errors.New("derive TLS key: the WireGuard identity is all zeros")
	}
	okm, err := hkdf.Key(sha256.New, identity[:], nil, keyInfo, 32*keyCandidates)
	if err != nil {
		return nil, fmt.Errorf("derive TLS key: %w", err)
	}
	for i := range keyCandidates {
		if key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), okm[32*i:32*(i+1)]); err == nil {
			return key, nil
		}
	}
	return nil, errors.New("derive TLS key: no valid P-256 scalar among the HKDF output")
}

// certLifetime is how long a SelfSigned cert is valid. Clients pin the public
// key and check no validity dates, and the cert is re-made at every start,
// so this only has to outlast one long-running process.
const certLifetime = 10 * 365 * 24 * time.Hour

// SelfSigned makes a self-signed server cert for key, valid from an hour
// before now. It names the web app's tunnel address and localhost, though no
// client verifies a name: they pin the key.
func SelfSigned(key *ecdsa.PrivateKey, now time.Time) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serial number: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "homelab-ops web app"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{wireguard.WebAppAddr.AsSlice(), net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("self-sign TLS cert: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse self-signed TLS cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// pinPrefix starts every fingerprint, as curl's --pinnedpubkey takes it.
const pinPrefix = "sha256//"

// Fingerprint is a public key's pin: "sha256//" and the standard base64 of
// the SHA-256 of its DER SubjectPublicKeyInfo. That is the form curl's
// --pinnedpubkey takes, and the one `bootstrap api --server-fingerprint`
// takes and the web app logs.
func Fingerprint(spkiDER []byte) string {
	sum := sha256.Sum256(spkiDER)
	return pinPrefix + base64.StdEncoding.EncodeToString(sum[:])
}

// PublicKeyFingerprint is Fingerprint of pub's DER SubjectPublicKeyInfo.
func PublicKeyFingerprint(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	return Fingerprint(der), nil
}

// ParseFingerprint checks that s is a Fingerprint: "sha256//" and the base64
// of 32 bytes. It returns s with surrounding space trimmed.
func ParseFingerprint(s string) (string, error) {
	s = strings.TrimSpace(s)
	b64, ok := strings.CutPrefix(s, pinPrefix)
	if !ok {
		return "", fmt.Errorf("fingerprint %q: want sha256//<base64>, as the web app logs it", s)
	}
	sum, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(sum) != sha256.Size {
		return "", fmt.Errorf("fingerprint %q: want sha256// and the base64 of 32 bytes", s)
	}
	return s, nil
}

// ParseCertsPEM parses every CERTIFICATE block in b. It fails if there is
// none, or on any other block: a private key in an operator-cert file is a
// mistake to stop on, not skip.
func ParseCertsPEM(b []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a %q PEM block, where only CERTIFICATE blocks belong (never a private key)", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate %d: %w", len(certs)+1, err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, errors.New("no PEM CERTIFICATE block")
	}
	return certs, nil
}

// NotOperatorMessage is the body of RequireOperator's 403. A client can
// match on it to tell the cert check's refusal from another 403, such as the
// tunnel's operator-peer filter.
const NotOperatorMessage = "forbidden: the API requires a listed operator client certificate"

// ServerConfig is the API listeners' TLS config: it serves cert and asks
// every client for a cert, without requiring or chain-verifying one, since
// operator certs are pinned rather than issued by a CA. A client that sends a
// cert must still prove it holds the key (the handshake's CertificateVerify),
// so the cert RequireOperator then checks is the client's own. Whether that
// cert is a listed operator cert is RequireOperator's call, over HTTP, not
// the handshake's: a refusal in a TLS 1.3 handshake reaches the client only
// after its own handshake has finished, where it can't be told from a
// dropped connection.
func ServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		// Ask for a cert but verify no chain: there is no CA, and
		// tls.VerifyClientCertIfGiven would chain-verify against ClientCAs
		// and so refuse every self-signed operator cert in the handshake.
		ClientAuth: tls.RequestClientCert,
	}
}

// RequireOperator wraps next, the whole handler a TLS API listener serves,
// so that only a request whose connection presented one of operators reaches
// it, matched by the SHA-256 of the whole cert as Incus matches it (§4).
// Every other request, with no cert or an unlisted one, or not over TLS at
// all, gets 403 Forbidden with NotOperatorMessage: one answer for each, so a
// client learns only that it isn't admitted. Wrapping the handler rather than
// listing routes means a route added later is covered too.
func RequireOperator(operators []*x509.Certificate, next http.Handler) http.Handler {
	allowed := make([][sha256.Size]byte, len(operators))
	for i, c := range operators {
		allowed[i] = sha256.Sum256(c.Raw)
	}
	listed := func(r *http.Request) bool {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			return false
		}
		got := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		for _, want := range allowed {
			if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !listed(r) {
			http.Error(w, NotOperatorMessage, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
