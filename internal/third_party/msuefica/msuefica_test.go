package msuefica

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"testing"
)

func TestUEFICAsMatchRecordedProvenance(t *testing.T) {
	certs := UEFICAs()
	if len(certs) != 2 {
		t.Fatalf("got %d certs, want 2", len(certs))
	}
	for _, c := range certs {
		sum := sha256.Sum256(c.DER)
		if got := hex.EncodeToString(sum[:]); got != c.SHA256 {
			t.Errorf("%s: SHA-256 %s, recorded %s", c.Name, got, c.SHA256)
		}
		parsed, err := x509.ParseCertificate(c.DER)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.Name, err)
		}
		if parsed.Subject.CommonName != c.Name {
			t.Errorf("subject CN %q, want %q", parsed.Subject.CommonName, c.Name)
		}
	}
}
