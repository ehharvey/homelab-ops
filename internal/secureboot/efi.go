package secureboot

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/ehharvey/homelab-ops/internal/third_party/msdbx"
	"github.com/ehharvey/homelab-ops/internal/third_party/msuefica"
)

// GUID is an EFI_GUID in its in-memory (wire) layout: the first three fields
// little-endian, the last eight bytes as written.
type GUID [16]byte

// ParseGUID parses the canonical 8-4-4-4-12 hex form.
func ParseGUID(s string) (GUID, error) {
	var g GUID
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 || len(s) != 36 {
		return g, fmt.Errorf("invalid GUID %q", s)
	}
	// Data1 (4 bytes), Data2 and Data3 (2 each) are stored little-endian.
	g[0], g[1], g[2], g[3] = raw[3], raw[2], raw[1], raw[0]
	g[4], g[5] = raw[5], raw[4]
	g[6], g[7] = raw[7], raw[6]
	copy(g[8:], raw[8:])
	return g, nil
}

func mustGUID(s string) GUID {
	g, err := ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// String returns the canonical 8-4-4-4-12 form.
func (g GUID) String() string {
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%x-%x",
		g[3], g[2], g[1], g[0], g[5], g[4], g[7], g[6], g[8:10], g[10:])
}

// NewGUID returns a random (version 4) GUID.
func NewGUID() (GUID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return GUID{}, fmt.Errorf("generate GUID: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return ParseGUID(fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]))
}

// GUIDs from the UEFI specification.
var (
	// certX509GUID is EFI_CERT_X509_GUID, the SignatureType of a list of
	// DER certificates.
	certX509GUID = mustGUID("a5c059a1-94e4-4aa7-87b5-ab155c2bf072")
	// certTypePKCS7GUID is EFI_CERT_TYPE_PKCS7_GUID, the CertType of an
	// authenticated variable's PKCS#7 signature.
	certTypePKCS7GUID = mustGUID("4aafd29d-68df-49ee-8aa9-347d375665a7")
	// GlobalVariableGUID is EFI_GLOBAL_VARIABLE, the vendor GUID of PK and KEK.
	GlobalVariableGUID = mustGUID("8be4df61-93ca-11d2-aa0d-00e098032b8c")
	// ImageSecurityDatabaseGUID is EFI_IMAGE_SECURITY_DATABASE_GUID, the
	// vendor GUID of db and dbx.
	ImageSecurityDatabaseGUID = mustGUID("d719b2cb-3d3a-4596-a3bc-dad00e67656f")
	// MicrosoftOwnerGUID is the SignatureOwner of Microsoft's db entries.
	MicrosoftOwnerGUID = mustGUID(msuefica.OwnerGUID)
)

// Variable attributes: NON_VOLATILE | BOOTSERVICE_ACCESS | RUNTIME_ACCESS |
// TIME_BASED_AUTHENTICATED_WRITE_ACCESS, what PK, KEK, db and dbx are written
// with.
const authVarAttributes uint32 = 0x01 | 0x02 | 0x04 | 0x20

// WIN_CERTIFICATE header values (UEFI spec, and the PE/COFF spec for PKCS#7).
const (
	winCertRevision            = 0x0200
	winCertTypeEFIGUID         = 0x0EF1
	winCertTypePKCSSignedData  = 0x0002
	winCertificateHeaderLength = 8
)

// SignatureList returns an EFI_SIGNATURE_LIST of type EFI_CERT_X509_GUID
// holding one DER certificate. Certificates differ in size and a list's
// entries share one SignatureSize, so each certificate gets its own list; a
// variable's value is the concatenation of its lists.
func SignatureList(owner GUID, certDER []byte) []byte {
	sigSize := 16 + len(certDER)
	b := make([]byte, 0, 28+sigSize)
	b = append(b, certX509GUID[:]...)
	b = binary.LittleEndian.AppendUint32(b, uint32(28+sigSize)) //nolint:gosec // a DER certificate is far below 4 GiB
	b = binary.LittleEndian.AppendUint32(b, 0)                  // SignatureHeaderSize
	b = binary.LittleEndian.AppendUint32(b, uint32(sigSize))    //nolint:gosec // as above
	b = append(b, owner[:]...)
	return append(b, certDER...)
}

// efiTime encodes t as an EFI_TIME in UTC. Time-based authenticated
// variables require Pad1, Nanosecond, TimeZone, Daylight and Pad2 to be zero.
func efiTime(t time.Time) []byte {
	t = t.UTC()
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:], uint16(t.Year())) //nolint:gosec // years fit in 16 bits
	for i, v := range []int{int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second()} {
		b[2+i] = byte(v) //nolint:gosec // G115: month, day, hour, minute and second all fit in a byte
	}
	return b
}

// signedVariablePayload is the byte string a time-based authenticated
// variable's signature covers: VariableName (UTF-16LE, no terminator),
// VendorGuid, Attributes, TimeStamp, then the new value.
func signedVariablePayload(name string, vendor GUID, timestamp, data []byte) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(name)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	b = append(b, vendor[:]...)
	b = binary.LittleEndian.AppendUint32(b, authVarAttributes)
	b = append(b, timestamp...)
	return append(b, data...)
}

// SignedVariable returns an EFI_VARIABLE_AUTHENTICATION_2 descriptor
// followed by data: the .auth file systemd-boot's auto-enroll (and
// efi-updatevar, and most firmware setup menus) writes with SetVariable. The
// signature is a detached PKCS#7 SignedData over the variable's name, vendor
// GUID, attributes, timestamp and value, the same shape efitools'
// sign-efi-sig-list produces.
func SignedVariable(name string, vendor GUID, data []byte, signer KeyPair, t time.Time) ([]byte, error) {
	ts := efiTime(t)
	digest := sha256.Sum256(signedVariablePayload(name, vendor, ts, data))
	p7, err := detachedSignedData(digest[:], signer)
	if err != nil {
		return nil, fmt.Errorf("sign %s: %w", name, err)
	}

	b := make([]byte, 0, 16+24+len(p7)+len(data))
	b = append(b, ts...)
	b = binary.LittleEndian.AppendUint32(b, uint32(winCertificateHeaderLength+16+len(p7))) //nolint:gosec // a signature is far below 4 GiB
	b = binary.LittleEndian.AppendUint16(b, winCertRevision)
	b = binary.LittleEndian.AppendUint16(b, winCertTypeEFIGUID)
	b = append(b, certTypePKCS7GUID[:]...)
	b = append(b, p7...)
	return append(b, data...), nil
}

// authenticatedValue returns the value an EFI_VARIABLE_AUTHENTICATION_2
// update carries: the signature lists after its descriptor. It checks that
// the descriptor is a PKCS#7 one and that the lists are well formed, but not
// the signature, because the caller re-signs the lists with its own key.
func authenticatedValue(auth []byte) ([]byte, error) {
	const descriptorMin = 16 + winCertificateHeaderLength + 16 // EFI_TIME, WIN_CERTIFICATE, CertType
	if len(auth) < descriptorMin {
		return nil, fmt.Errorf("%d bytes is shorter than an EFI_VARIABLE_AUTHENTICATION_2 descriptor", len(auth))
	}
	if rev, typ := binary.LittleEndian.Uint16(auth[20:]), binary.LittleEndian.Uint16(auth[22:]); rev != winCertRevision || typ != winCertTypeEFIGUID {
		return nil, fmt.Errorf("WIN_CERTIFICATE revision %#x type %#x, want %#x %#x", rev, typ, winCertRevision, winCertTypeEFIGUID)
	}
	var certType GUID
	copy(certType[:], auth[24:40])
	if certType != certTypePKCS7GUID {
		return nil, fmt.Errorf("CertType %s, want EFI_CERT_TYPE_PKCS7_GUID", certType)
	}
	length := uint64(binary.LittleEndian.Uint32(auth[16:]))
	if length < winCertificateHeaderLength+16 || 16+length > uint64(len(auth)) {
		return nil, fmt.Errorf("WIN_CERTIFICATE length %d does not fit in %d bytes", length, len(auth))
	}
	value := auth[16+length:]
	if err := checkSignatureLists(value); err != nil {
		return nil, err
	}
	return value, nil
}

// checkSignatureLists checks that b is one or more EFI_SIGNATURE_LISTs whose
// sizes add up.
func checkSignatureLists(b []byte) error {
	if len(b) == 0 {
		return errors.New("no signature lists")
	}
	for off := 0; off < len(b); {
		rest := b[off:]
		if len(rest) < 28 {
			return fmt.Errorf("truncated EFI_SIGNATURE_LIST header at offset %d", off)
		}
		listSize := uint64(binary.LittleEndian.Uint32(rest[16:]))
		headerSize := uint64(binary.LittleEndian.Uint32(rest[20:]))
		sigSize := uint64(binary.LittleEndian.Uint32(rest[24:]))
		if sigSize < 16 || listSize < 28+headerSize || listSize > uint64(len(rest)) || (listSize-28-headerSize)%sigSize != 0 {
			return fmt.Errorf("malformed EFI_SIGNATURE_LIST at offset %d: list %d, header %d, signature %d bytes", off, listSize, headerSize, sigSize)
		}
		off += int(listSize) //nolint:gosec // G115: listSize was just checked to be at most len(rest)
	}
	return nil
}

// Payloads are the enrollment payloads for one hierarchy: each variable's
// signature lists (.esl) and its signed update (.auth).
type Payloads struct {
	PKESL, KEKESL, DBESL    []byte
	PKAuth, KEKAuth, DBAuth []byte
	// DBXESL and DBXAuth are Microsoft's x64 dbx, re-signed by KEK. They are
	// set only when db includes Microsoft's UEFI CA, and nil otherwise.
	DBXESL, DBXAuth []byte
}

// PayloadOptions configures Hierarchy.Payloads.
type PayloadOptions struct {
	// IncludeMicrosoftCA adds Microsoft's third-party UEFI CA certificates
	// (2011 and 2023) to db, so option ROMs signed under them still run.
	// The default in the bootstrap CLI; §31 makes it an operator choice.
	//
	// It also builds dbx from Microsoft's x64 list of revoked binaries
	// (internal/third_party/msdbx). That CA has signed shims and boot
	// loaders that were later revoked, and clearing a machine's factory
	// keys can empty its dbx, so trusting the CA without its revocations
	// would let those binaries boot again.
	IncludeMicrosoftCA bool
	// Time is the authenticated variables' timestamp. Firmware rejects an
	// update older than the variable's current one. Zero means time.Now().
	Time time.Time
}

// Payloads builds the PK, KEK and db enrollment payloads, and dbx's when db
// includes Microsoft's UEFI CA. PK is signed by PK (a machine in Setup Mode
// accepts it unsigned, but a signed one also works from User Mode with the
// same PK), KEK by PK, and db and dbx by KEK.
func (h *Hierarchy) Payloads(opts PayloadOptions) (*Payloads, error) {
	t := opts.Time
	if t.IsZero() {
		t = time.Now()
	}

	p := &Payloads{
		PKESL:  SignatureList(h.OwnerGUID, h.PK.Cert.Raw),
		KEKESL: SignatureList(h.OwnerGUID, h.KEK.Cert.Raw),
		DBESL:  SignatureList(h.OwnerGUID, h.DB.Cert.Raw),
	}
	if opts.IncludeMicrosoftCA {
		for _, c := range msuefica.UEFICAs() {
			p.DBESL = append(p.DBESL, SignatureList(MicrosoftOwnerGUID, c.DER)...)
		}
		dbx, err := authenticatedValue(msdbx.X64())
		if err != nil {
			return nil, fmt.Errorf("read Microsoft's x64 dbx: %w", err)
		}
		p.DBXESL = bytes.Clone(dbx)
	}

	var err error
	if p.PKAuth, err = SignedVariable("PK", GlobalVariableGUID, p.PKESL, h.PK, t); err != nil {
		return nil, err
	}
	if p.KEKAuth, err = SignedVariable("KEK", GlobalVariableGUID, p.KEKESL, h.PK, t); err != nil {
		return nil, err
	}
	if p.DBAuth, err = SignedVariable("db", ImageSecurityDatabaseGUID, p.DBESL, h.KEK, t); err != nil {
		return nil, err
	}
	if p.DBXESL != nil {
		if p.DBXAuth, err = SignedVariable("dbx", ImageSecurityDatabaseGUID, p.DBXESL, h.KEK, t); err != nil {
			return nil, err
		}
	}
	return p, nil
}
