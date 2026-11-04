package secureboot

import (
	"crypto/rsa"
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

// testHierarchy generates one hierarchy per test binary: RSA key generation
// is the slow part of these tests.
var testHierarchy = func() func(t *testing.T) *Hierarchy {
	var h *Hierarchy
	return func(t *testing.T) *Hierarchy {
		t.Helper()
		if h == nil {
			var err error
			if h, err = Generate(Options{ValidityDays: 30}); err != nil {
				t.Fatalf("Generate: %v", err)
			}
		}
		return h
	}
}()

func TestGenerateBuildsAPKKEKDBChain(t *testing.T) {
	h := testHierarchy(t)

	for name, kp := range map[string]KeyPair{"PK": h.PK, "KEK": h.KEK, "db": h.DB} {
		pub, ok := kp.Cert.PublicKey.(*rsa.PublicKey)
		if !ok || pub.N.BitLen() != RSABits {
			t.Errorf("%s: want an RSA-%d key, got %T", name, RSABits, kp.Cert.PublicKey)
		}
		if kp.Cert.SignatureAlgorithm != x509.SHA256WithRSA {
			t.Errorf("%s: signature algorithm %v, want SHA256WithRSA", name, kp.Cert.SignatureAlgorithm)
		}
		if !strings.HasPrefix(kp.Cert.Subject.CommonName, Organization+" ") {
			t.Errorf("%s: CommonName %q", name, kp.Cert.Subject.CommonName)
		}
	}
	if !h.PK.Cert.IsCA || !h.KEK.Cert.IsCA || h.DB.Cert.IsCA {
		t.Errorf("IsCA: PK %v KEK %v db %v, want true true false", h.PK.Cert.IsCA, h.KEK.Cert.IsCA, h.DB.Cert.IsCA)
	}

	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(h.PK.Cert)
	intermediates.AddCert(h.KEK.Cert)
	chains, err := h.DB.Cert.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	})
	if err != nil {
		t.Fatalf("db does not chain to PK through KEK: %v", err)
	}
	if got := len(chains[0]); got != 3 {
		t.Errorf("chain length %d, want 3 (db, KEK, PK)", got)
	}
}

func TestGenerateGivesEachHierarchyItsOwnOwnerGUID(t *testing.T) {
	a := testHierarchy(t)
	b, err := Generate(Options{ValidityDays: 1})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if a.OwnerGUID == b.OwnerGUID {
		t.Errorf("two hierarchies share owner GUID %s", a.OwnerGUID)
	}
	if v := b.OwnerGUID[7] >> 4; v != 4 {
		t.Errorf("owner GUID %s is version %d, want 4", b.OwnerGUID, v)
	}
}

func TestGenerateHonoursValidity(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h, err := Generate(Options{ValidityDays: 10, Now: now})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !h.DB.Cert.NotBefore.Equal(now) || !h.DB.Cert.NotAfter.Equal(now.AddDate(0, 0, 10)) {
		t.Errorf("db validity %v..%v", h.DB.Cert.NotBefore, h.DB.Cert.NotAfter)
	}
	if _, err := Generate(Options{}); err == nil {
		t.Error("Generate with zero ValidityDays: want an error")
	}
}

func TestLoadKeyPairRoundTripsAndRejectsAMismatch(t *testing.T) {
	h := testHierarchy(t)
	keyPEM, err := h.DB.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}
	kp, err := LoadKeyPair(h.DB.CertPEM(), keyPEM)
	if err != nil {
		t.Fatalf("LoadKeyPair: %v", err)
	}
	if !kp.Cert.Equal(h.DB.Cert) {
		t.Error("loaded certificate differs")
	}

	if _, err := LoadKeyPair(h.KEK.CertPEM(), keyPEM); err == nil {
		t.Error("LoadKeyPair(KEK cert, db key): want a mismatch error")
	}
	if _, err := LoadKeyPair(keyPEM, keyPEM); err == nil {
		t.Error("LoadKeyPair with a key as the cert: want an error")
	}
}

func TestGUIDWireLayout(t *testing.T) {
	// EFI_GLOBAL_VARIABLE as it appears in memory and in .auth files.
	want := []byte{0x61, 0xdf, 0xe4, 0x8b, 0xca, 0x93, 0xd2, 0x11, 0xaa, 0x0d, 0x00, 0xe0, 0x98, 0x03, 0x2b, 0x8c}
	if string(GlobalVariableGUID[:]) != string(want) {
		t.Errorf("EFI_GLOBAL_VARIABLE bytes % x, want % x", GlobalVariableGUID[:], want)
	}
	if got := GlobalVariableGUID.String(); got != "8be4df61-93ca-11d2-aa0d-00e098032b8c" {
		t.Errorf("String() = %s", got)
	}
	for _, bad := range []string{"", "8be4df61-93ca-11d2-aa0d-00e098032b8", "8be4df6193ca11d2aa0d00e098032b8cxxxx"} {
		if _, err := ParseGUID(bad); err == nil {
			t.Errorf("ParseGUID(%q): want an error", bad)
		}
	}
}
