package secureboot

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/third_party/msdbx"
	"github.com/ehharvey/homelab-ops/internal/third_party/msuefica"
)

type eslEntry struct {
	owner GUID
	der   []byte
}

// parseESLs reads a variable value made of EFI_SIGNATURE_LISTs of X.509
// certificates.
func parseESLs(t *testing.T, b []byte) []eslEntry {
	t.Helper()
	var out []eslEntry
	for len(b) > 0 {
		if len(b) < 28 {
			t.Fatalf("truncated EFI_SIGNATURE_LIST header (%d bytes)", len(b))
		}
		var typ GUID
		copy(typ[:], b)
		if typ != certX509GUID {
			t.Fatalf("SignatureType %s, want EFI_CERT_X509_GUID", typ)
		}
		listSize := int(binary.LittleEndian.Uint32(b[16:]))
		headerSize := int(binary.LittleEndian.Uint32(b[20:]))
		sigSize := int(binary.LittleEndian.Uint32(b[24:]))
		if headerSize != 0 || listSize > len(b) || (listSize-28)%sigSize != 0 {
			t.Fatalf("bad list sizes: list %d header %d sig %d (have %d)", listSize, headerSize, sigSize, len(b))
		}
		for sigs := b[28:listSize]; len(sigs) > 0; sigs = sigs[sigSize:] {
			var e eslEntry
			copy(e.owner[:], sigs)
			e.der = sigs[16:sigSize]
			out = append(out, e)
		}
		b = b[listSize:]
	}
	return out
}

func TestSignatureListLayout(t *testing.T) {
	owner := mustGUID("11111111-2222-3333-4444-555555555555")
	der := []byte{1, 2, 3}
	got := SignatureList(owner, der)
	want := append([]byte{
		0xa1, 0x59, 0xc0, 0xa5, 0xe4, 0x94, 0xa7, 0x4a, 0x87, 0xb5, 0xab, 0x15, 0x5c, 0x2b, 0xf0, 0x72, // EFI_CERT_X509_GUID
		47, 0, 0, 0, // SignatureListSize: 28 + 16 + 3
		0, 0, 0, 0, // SignatureHeaderSize
		19, 0, 0, 0, // SignatureSize: 16 + 3
	}, owner[:]...)
	want = append(want, der...)
	if !bytes.Equal(got, want) {
		t.Errorf("SignatureList:\n got % x\nwant % x", got, want)
	}
}

func TestPayloadsDBIncludesMicrosoftUEFICAByDefaultChoice(t *testing.T) {
	h := testHierarchy(t)
	ms := msuefica.UEFICAs()

	for _, tc := range []struct {
		name      string
		includeMS bool
		want      []eslEntry
	}{
		{"with Microsoft CA", true, []eslEntry{
			{h.OwnerGUID, h.DB.Cert.Raw},
			{MicrosoftOwnerGUID, ms[0].DER},
			{MicrosoftOwnerGUID, ms[1].DER},
		}},
		{"without Microsoft CA", false, []eslEntry{{h.OwnerGUID, h.DB.Cert.Raw}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := h.Payloads(PayloadOptions{IncludeMicrosoftCA: tc.includeMS})
			if err != nil {
				t.Fatalf("Payloads: %v", err)
			}
			got := parseESLs(t, p.DBESL)
			if len(got) != len(tc.want) {
				t.Fatalf("db has %d entries, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].owner != tc.want[i].owner || !bytes.Equal(got[i].der, tc.want[i].der) {
					t.Errorf("db entry %d: owner %s, want %s (or the certificate differs)", i, got[i].owner, tc.want[i].owner)
				}
			}
			for name, esl := range map[string][]byte{"PK": p.PKESL, "KEK": p.KEKESL} {
				entries := parseESLs(t, esl)
				if len(entries) != 1 || entries[0].owner != h.OwnerGUID {
					t.Errorf("%s: want one entry owned by %s, got %d", name, h.OwnerGUID, len(entries))
				}
			}
		})
	}
}

// checkSignedVariable parses an .auth payload and verifies its signature
// against signer, independently of SignedVariable.
func checkSignedVariable(t *testing.T, auth []byte, name string, vendor GUID, wantData []byte, signer *x509.Certificate, ts time.Time) {
	t.Helper()
	if len(auth) < 40 {
		t.Fatalf("%s.auth: %d bytes", name, len(auth))
	}
	if !bytes.Equal(auth[:16], efiTime(ts)) {
		t.Errorf("%s.auth: timestamp % x, want % x", name, auth[:16], efiTime(ts))
	}
	length := int(binary.LittleEndian.Uint32(auth[16:]))
	if rev, typ := binary.LittleEndian.Uint16(auth[20:]), binary.LittleEndian.Uint16(auth[22:]); rev != 0x0200 || typ != 0x0EF1 {
		t.Errorf("%s.auth: WIN_CERTIFICATE revision %#x type %#x", name, rev, typ)
	}
	var certType GUID
	copy(certType[:], auth[24:40])
	if certType != certTypePKCS7GUID {
		t.Errorf("%s.auth: CertType %s, want EFI_CERT_TYPE_PKCS7_GUID", name, certType)
	}
	p7 := auth[40 : 16+length]
	if data := auth[16+length:]; !bytes.Equal(data, wantData) {
		t.Errorf("%s.auth: data after the descriptor is not the signature list", name)
	}

	// EDK2 checks the digest algorithm at a fixed offset into CertData,
	// which only holds for a bare SignedData with two-byte lengths.
	sha256OID := []byte{0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x01}
	if p7[0] != 0x30 || p7[1] != 0x82 || !bytes.Equal(p7[11:22], sha256OID) {
		t.Errorf("%s.auth: CertData does not start like a bare SHA-256 SignedData: % x", name, p7[:24])
	}

	sd := parseSignedData(t, p7)
	if !sd.contentType.Equal(oidData) || sd.content != nil || sd.signedAttrs != nil {
		t.Errorf("%s.auth: want detached id-data content and no signed attributes", name)
	}
	if !bytes.Equal(sd.certDER, signer.Raw) {
		t.Errorf("%s.auth: embedded certificate is not %q", name, signer.Subject.CommonName)
	}
	digest := sha256.Sum256(signedVariablePayload(name, vendor, efiTime(ts), wantData))
	if err := rsa.VerifyPKCS1v15(signer.PublicKey.(*rsa.PublicKey), crypto.SHA256, digest[:], sd.signature); err != nil {
		t.Errorf("%s.auth: signature does not verify with %q: %v", name, signer.Subject.CommonName, err)
	}
}

func TestPayloadsAreSignedUpTheHierarchy(t *testing.T) {
	h := testHierarchy(t)
	ts := time.Date(2026, 11, 4, 5, 6, 7, 8, time.FixedZone("x", 3600))
	p, err := h.Payloads(PayloadOptions{IncludeMicrosoftCA: true, Time: ts})
	if err != nil {
		t.Fatalf("Payloads: %v", err)
	}
	checkSignedVariable(t, p.PKAuth, "PK", GlobalVariableGUID, p.PKESL, h.PK.Cert, ts)
	checkSignedVariable(t, p.KEKAuth, "KEK", GlobalVariableGUID, p.KEKESL, h.PK.Cert, ts)
	checkSignedVariable(t, p.DBAuth, "db", ImageSecurityDatabaseGUID, p.DBESL, h.KEK.Cert, ts)
	checkSignedVariable(t, p.DBXAuth, "dbx", ImageSecurityDatabaseGUID, p.DBXESL, h.KEK.Cert, ts)
}

// certSHA256GUID is EFI_CERT_SHA256_GUID, the SignatureType of Microsoft's
// dbx entries (Authenticode SHA-256 hashes).
var certSHA256GUID = mustGUID("c1c41626-504c-4092-aca9-41f936934328")

func TestPayloadsDBXIsMicrosoftsX64ListOnlyWithTheMicrosoftCA(t *testing.T) {
	h := testHierarchy(t)
	p, err := h.Payloads(PayloadOptions{IncludeMicrosoftCA: true})
	if err != nil {
		t.Fatalf("Payloads: %v", err)
	}
	// The value is Microsoft's, byte for byte, after its descriptor.
	ms := msdbx.X64()
	if want := ms[16+binary.LittleEndian.Uint32(ms[16:]):]; !bytes.Equal(p.DBXESL, want) {
		t.Fatalf("dbx.esl (%d bytes) is not the signature lists of Microsoft's x64 dbx (%d bytes)", len(p.DBXESL), len(want))
	}
	hashes := 0
	for b := p.DBXESL; len(b) > 0; {
		var typ GUID
		copy(typ[:], b)
		listSize := binary.LittleEndian.Uint32(b[16:])
		headerSize := binary.LittleEndian.Uint32(b[20:])
		sigSize := binary.LittleEndian.Uint32(b[24:])
		if typ != certSHA256GUID || sigSize != 16+32 {
			t.Fatalf("list type %s, signature size %d: want SHA-256 entries", typ, sigSize)
		}
		for sigs := b[28+headerSize : listSize]; len(sigs) > 0; sigs = sigs[sigSize:] {
			var owner GUID
			copy(owner[:], sigs)
			if owner != MicrosoftOwnerGUID {
				t.Fatalf("entry %d owner %s, want Microsoft's %s", hashes, owner, MicrosoftOwnerGUID)
			}
			hashes++
		}
		b = b[listSize:]
	}
	if hashes != msdbx.X64Hashes {
		t.Errorf("dbx holds %d hashes, want %d", hashes, msdbx.X64Hashes)
	}
	// Re-signing must not reach into the embedded file.
	p.DBXESL[len(p.DBXESL)-1] ^= 0xff
	if p2, err := h.Payloads(PayloadOptions{IncludeMicrosoftCA: true}); err != nil || bytes.Equal(p.DBXESL, p2.DBXESL) {
		t.Errorf("modifying one Payloads' dbx.esl changed the next one's (err %v)", err)
	}

	p, err = h.Payloads(PayloadOptions{})
	if err != nil {
		t.Fatalf("Payloads without the Microsoft CA: %v", err)
	}
	if p.DBXESL != nil || p.DBXAuth != nil {
		t.Errorf("without the Microsoft CA: want no dbx, got %d and %d bytes", len(p.DBXESL), len(p.DBXAuth))
	}
}

func TestAuthenticatedValueRoundTripsAndRejectsMalformedUpdates(t *testing.T) {
	h := testHierarchy(t)
	esl := SignatureList(h.OwnerGUID, h.DB.Cert.Raw)
	auth, err := SignedVariable("db", ImageSecurityDatabaseGUID, esl, h.KEK, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := authenticatedValue(auth); err != nil || !bytes.Equal(got, esl) {
		t.Fatalf("authenticatedValue(SignedVariable(esl)) = %d bytes, %v; want esl back", len(got), err)
	}

	descLen := 16 + int(binary.LittleEndian.Uint32(auth[16:]))
	mutate := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(auth)) }
	for name, bad := range map[string][]byte{
		"shorter than a descriptor":  auth[:39],
		"not WIN_CERT_TYPE_EFI_GUID": mutate(func(b []byte) []byte { b[22] = 0x02; return b }),
		"not a PKCS#7 CertType":      mutate(func(b []byte) []byte { b[24] ^= 0xff; return b }),
		"length past the end":        mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[16:], uint32(len(b))); return b }),
		"no signature lists":         auth[:descLen],
		"truncated list header":      auth[:descLen+27],
		"list size past the end":     mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[descLen+16:], uint32(len(esl)+1)); return b }),
		// One byte more than the list's body, so it can never divide it.
		"entries not a whole number": mutate(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[descLen+24:], uint32(len(esl)-28+1)); return b }),
	} {
		if _, err := authenticatedValue(bad); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestEFITimeIsUTCWithZeroedFields(t *testing.T) {
	got := efiTime(time.Date(2026, 11, 4, 5, 6, 7, 999, time.FixedZone("x", 3600)))
	want := []byte{0xea, 0x07, 11, 4, 4, 6, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Errorf("efiTime = % x, want % x", got, want)
	}
}

func TestWriteSeparatesOfflineKeysAndRefusesToOverwrite(t *testing.T) {
	h := testHierarchy(t)
	p, err := h.Payloads(PayloadOptions{})
	if err != nil {
		t.Fatalf("Payloads: %v", err)
	}
	dir := t.TempDir()
	outDir, offDir := filepath.Join(dir, "sb"), filepath.Join(dir, "offline")
	if _, err := Write(outDir, offDir, h, p, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	offline, err := os.ReadDir(offDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range offline {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != KEKKeyFile || names[1] != PKKeyFile {
		t.Errorf("offline dir holds %v, want only %s and %s", names, KEKKeyFile, PKKeyFile)
	}
	for _, f := range []string{PKKeyFile, KEKKeyFile} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err == nil {
			t.Errorf("%s is in the output dir too", f)
		}
	}

	for _, f := range []string{filepath.Join(offDir, PKKeyFile), filepath.Join(offDir, KEKKeyFile), filepath.Join(outDir, DBKeyFile)} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
		}
	}
	for _, f := range []string{PKCertFile, KEKCertFile, DBCertFile, PKESLFile, KEKESLFile, DBESLFile, PKAuthFile, KEKAuthFile, DBAuthFile, OwnerGUIDFile} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	auth, err := os.ReadFile(filepath.Join(outDir, DBAuthFile))
	if err != nil || !bytes.Equal(auth, p.DBAuth) {
		t.Errorf("db.auth on disk differs from the payload (%v)", err)
	}

	for _, f := range []string{DBXESLFile, DBXAuthFile} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err == nil {
			t.Errorf("%s written without the Microsoft CA", f)
		}
	}

	if _, err := Write(outDir, offDir, h, p, false); err == nil {
		t.Error("second Write without force: want an error")
	}
	if _, err := Write(outDir, offDir, h, p, true); err != nil {
		t.Errorf("Write with force: %v", err)
	}
}

func TestWriteWritesDBXWithTheMicrosoftCAAndForceClearsAStaleOne(t *testing.T) {
	h := testHierarchy(t)
	withMS, err := h.Payloads(PayloadOptions{IncludeMicrosoftCA: true})
	if err != nil {
		t.Fatal(err)
	}
	noMS, err := h.Payloads(PayloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outDir, offDir := filepath.Join(dir, "sb"), filepath.Join(dir, "offline")
	if _, err := Write(outDir, offDir, h, withMS, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for f, want := range map[string][]byte{DBXESLFile: withMS.DBXESL, DBXAuthFile: withMS.DBXAuth} {
		if got, err := os.ReadFile(filepath.Join(outDir, f)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s on disk differs from the payload (%v)", f, err)
		}
	}

	// A run without the Microsoft CA into a fresh offline dir still finds the
	// earlier dbx in the output dir: refused without force, removed with it.
	offDir2 := filepath.Join(dir, "offline2")
	if _, err := Write(outDir, offDir2, h, noMS, false); err == nil {
		t.Error("Write without force over a stale dbx.auth: want an error")
	}
	if _, err := Write(outDir, offDir2, h, noMS, true); err != nil {
		t.Fatalf("Write with force: %v", err)
	}
	for _, f := range []string{DBXESLFile, DBXAuthFile} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err == nil {
			t.Errorf("%s left behind by a --force run without the Microsoft CA", f)
		}
	}
}

func TestWriteLeavesThePreviousSetWhenItCannotReplaceATarget(t *testing.T) {
	h := testHierarchy(t)
	p, err := h.Payloads(PayloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	outDir, offDir := filepath.Join(dir, "sb"), filepath.Join(dir, "offline")
	if _, err := Write(outDir, offDir, h, p, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	pkKey := filepath.Join(offDir, PKKeyFile)
	before, err := os.ReadFile(pkKey)
	if err != nil {
		t.Fatal(err)
	}

	// The last target is now a directory, so the second run fails after
	// the offline keys would have been replaced if Write wrote in place.
	guid := filepath.Join(outDir, OwnerGUIDFile)
	if err := os.Remove(guid); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(guid, 0o750); err != nil {
		t.Fatal(err)
	}
	other, err := Generate(Options{ValidityDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := other.Payloads(PayloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(outDir, offDir, other, p2, true); err == nil {
		t.Fatal("Write over a directory target: want an error")
	}
	if after, err := os.ReadFile(pkKey); err != nil || !bytes.Equal(after, before) {
		t.Errorf("PK.key changed although the write failed (%v)", err)
	}
}
