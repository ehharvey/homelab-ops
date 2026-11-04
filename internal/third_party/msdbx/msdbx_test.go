package msdbx

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestX64MatchesRecordedProvenance(t *testing.T) {
	sum := sha256.Sum256(X64())
	if got := hex.EncodeToString(sum[:]); got != X64SHA256 {
		t.Errorf("dbx_x64.efiauth2: SHA-256 %s, recorded %s", got, X64SHA256)
	}
}
