package secureboot

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"
)

// testPE is a synthetic, unsigned PE image and the offsets the Authenticode
// hash depends on, written down independently of parsePE.
type testPE struct {
	image       []byte
	checksumOff int
	certDirOff  int
	sections    [][2]int // {offset, size}, in file order
}

// buildTestPE lays out a minimal EFI application: DOS header, PE signature,
// COFF header, optional header, two sections listed out of file order, and
// trailing bytes past the last section (which the hash must include).
func buildTestPE(t *testing.T, plus bool, trailer int) testPE {
	t.Helper()
	const peOff, sizeOfHeaders, fileAlign = 0x40, 0x200, 0x200
	optSize, dirsOff := 96+16*8, 96
	if plus {
		optSize, dirsOff = 112+16*8, 112
	}
	opt := peOff + 4 + 20
	secTable := opt + optSize

	b := make([]byte, sizeOfHeaders+2*fileAlign+trailer)
	le16 := func(off, v int) { binary.LittleEndian.PutUint16(b[off:], uint16(v)) } //nolint:gosec // test layout values are small
	le32 := func(off, v int) { binary.LittleEndian.PutUint32(b[off:], uint32(v)) } //nolint:gosec // as above

	copy(b, "MZ")
	le32(0x3c, peOff)
	copy(b[peOff:], "PE\x00\x00")
	machine := 0x14c
	if plus {
		machine = 0x8664
	}
	le16(peOff+4, machine)
	le16(peOff+4+2, 2)        // NumberOfSections
	le16(peOff+4+16, optSize) // SizeOfOptionalHeader
	le16(peOff+4+18, 0x0102)  // Characteristics: executable, 32-bit word machine
	if plus {
		le16(opt, pe32PlusMagic)
	} else {
		le16(opt, pe32Magic)
	}
	le32(opt+32, 0x1000)        // SectionAlignment
	le32(opt+36, fileAlign)     // FileAlignment
	le32(opt+56, 0x3000)        // SizeOfImage
	le32(opt+60, sizeOfHeaders) // SizeOfHeaders
	le32(opt+64, 0xdeadbeef)    // CheckSum, excluded from the hash
	le16(opt+68, 10)            // Subsystem: EFI application
	le32(opt+dirsOff-4, 16)     // NumberOfRvaAndSizes

	// Section table: .data (second in the file) is listed before .text.
	secs := []struct {
		name     string
		va, off  int
		contents byte
	}{
		{".data", 0x2000, sizeOfHeaders + fileAlign, 0xdd},
		{".text", 0x1000, sizeOfHeaders, 0xcc},
	}
	for i, s := range secs {
		hdr := secTable + i*40
		copy(b[hdr:], s.name)
		le32(hdr+8, fileAlign) // VirtualSize
		le32(hdr+12, s.va)
		le32(hdr+16, fileAlign) // SizeOfRawData
		le32(hdr+20, s.off)
		for j := range fileAlign {
			b[s.off+j] = s.contents ^ byte(j)
		}
	}
	for i := range trailer {
		b[sizeOfHeaders+2*fileAlign+i] = byte(0x40 + i)
	}

	return testPE{
		image:       b,
		checksumOff: opt + 64,
		certDirOff:  opt + dirsOff + certTableIndex*8,
		sections:    [][2]int{{sizeOfHeaders, fileAlign}, {sizeOfHeaders + fileAlign, fileAlign}},
	}
}

// expectedDigest hashes image per the PE spec using the layout tp records,
// treating everything from certOff on as the certificate table.
func (tp testPE) expectedDigest(image []byte, certOff int) []byte {
	h := sha256.New()
	h.Write(image[:tp.checksumOff])
	h.Write(image[tp.checksumOff+4 : tp.certDirOff])
	h.Write(image[tp.certDirOff+8 : 0x200])
	end := 0x200
	for _, s := range tp.sections {
		h.Write(image[s[0] : s[0]+s[1]])
		end = s[0] + s[1]
	}
	h.Write(image[end:certOff])
	return h.Sum(nil)
}

func TestSignPEProducesAVerifiableAuthenticodeSignature(t *testing.T) {
	h := testHierarchy(t)
	for _, tc := range []struct {
		name    string
		plus    bool
		trailer int
	}{
		{"PE32+ with an unaligned trailer", true, 13},
		{"PE32 with no trailer", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := buildTestPE(t, tc.plus, tc.trailer)
			if _, err := pe.NewFile(bytes.NewReader(tp.image)); err != nil {
				t.Fatalf("test image does not parse with debug/pe: %v", err)
			}

			signed, err := SignPE(tp.image, h.DB)
			if err != nil {
				t.Fatalf("SignPE: %v", err)
			}

			padded := (len(tp.image) + 7) &^ 7
			if !bytes.Equal(signed[:tp.checksumOff], tp.image[:tp.checksumOff]) ||
				!bytes.Equal(signed[tp.checksumOff+4:tp.certDirOff], tp.image[tp.checksumOff+4:tp.certDirOff]) ||
				!bytes.Equal(signed[tp.certDirOff+8:len(tp.image)], tp.image[tp.certDirOff+8:]) ||
				!bytes.Equal(signed[len(tp.image):padded], make([]byte, padded-len(tp.image))) {
				t.Fatal("SignPE changed bytes other than CheckSum, the Certificate Table entry and zero padding")
			}
			if _, err := pe.NewFile(bytes.NewReader(signed)); err != nil {
				t.Fatalf("signed image does not parse with debug/pe: %v", err)
			}

			certOff := int(binary.LittleEndian.Uint32(signed[tp.certDirOff:]))
			certSize := int(binary.LittleEndian.Uint32(signed[tp.certDirOff+4:]))
			if certOff != padded || certOff+certSize != len(signed) || certOff%8 != 0 || certSize%8 != 0 {
				t.Fatalf("Certificate Table at %d size %d in a %d-byte image (unsigned image padded to %d)", certOff, certSize, len(signed), padded)
			}
			length := int(binary.LittleEndian.Uint32(signed[certOff:]))
			if rev, typ := binary.LittleEndian.Uint16(signed[certOff+4:]), binary.LittleEndian.Uint16(signed[certOff+6:]); rev != 0x0200 || typ != 0x0002 {
				t.Errorf("WIN_CERTIFICATE revision %#x type %#x, want 0x200 0x2", rev, typ)
			}
			if length > certSize || (length+7)&^7 != certSize {
				t.Errorf("WIN_CERTIFICATE dwLength %d for a %d-byte table", length, certSize)
			}

			var ci, oid, sd cryptobyte.String
			s := cryptobyte.String(signed[certOff+8 : certOff+length])
			if !s.ReadASN1(&ci, asn1.SEQUENCE) || !s.Empty() {
				t.Fatal("signature is not one ContentInfo")
			}
			if !ci.ReadASN1(&oid, asn1.OBJECT_IDENTIFIER) {
				t.Fatal("ContentInfo: no contentType")
			}
			if !bytes.Equal(oid, []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x07, 0x02}) {
				t.Fatalf("ContentInfo contentType % x, want signedData", []byte(oid))
			}
			if !ci.ReadASN1(&sd, tagContext0) {
				t.Fatal("ContentInfo: no [0] content")
			}
			p := parseSignedData(t, sd)
			if !p.contentType.Equal(oidSPCIndirectData) {
				t.Errorf("content type %v, want SPC_INDIRECT_DATA", p.contentType)
			}
			if !bytes.Equal(p.certDER, h.DB.Cert.Raw) {
				t.Error("embedded certificate is not the db certificate")
			}

			// The SpcIndirectDataContent's digest is the image's Authenticode hash,
			// both as computed from the recorded layout and as recomputed over the
			// signed image (which must exclude CheckSum and the table again).
			var spc, spcInner, attrTV, digestInfo, alg, imageDigest cryptobyte.String
			content := cryptobyte.String(p.content)
			if !content.ReadASN1Element(&spc, asn1.SEQUENCE) || !content.Empty() {
				t.Fatal("content is not one SpcIndirectDataContent")
			}
			spcBody := spc
			if !spcBody.ReadASN1(&spcInner, asn1.SEQUENCE) ||
				!spcInner.ReadASN1(&attrTV, asn1.SEQUENCE) || !spcInner.ReadASN1(&digestInfo, asn1.SEQUENCE) ||
				!digestInfo.ReadASN1(&alg, asn1.SEQUENCE) || !digestInfo.ReadASN1(&imageDigest, asn1.OCTET_STRING) {
				t.Fatal("bad SpcIndirectDataContent")
			}
			paddedImage := signed[:padded]
			if want := tp.expectedDigest(paddedImage, padded); !bytes.Equal(imageDigest, want) {
				t.Errorf("signed digest %x, want %x", []byte(imageDigest), want)
			}
			img, err := parsePE(signed)
			if err != nil {
				t.Fatalf("parsePE(signed): %v", err)
			}
			if got := authenticodeDigest(signed, img); !bytes.Equal(got, imageDigest) {
				t.Errorf("hash of the signed image %x differs from the signed digest %x", got, []byte(imageDigest))
			}

			// messageDigest covers the SpcIndirectDataContent's value octets, and
			// the signature covers the signed attributes as a SET.
			var innerSpc cryptobyte.String
			var tag asn1.Tag
			spcAgain := spc
			spcAgain.ReadAnyASN1(&innerSpc, &tag)
			wantMD := sha256.Sum256(innerSpc)
			mdValue := cryptobyte.String(signedAttribute(t, p.signedAttrs, oidMessageDigest))
			var md cryptobyte.String
			if !mdValue.ReadASN1(&md, asn1.OCTET_STRING) || !bytes.Equal(md, wantMD[:]) {
				t.Errorf("messageDigest %x, want %x", []byte(md), wantMD)
			}
			ctValue := cryptobyte.String(signedAttribute(t, p.signedAttrs, oidContentType))
			var gotCT cryptobyte.String
			if !ctValue.ReadASN1(&gotCT, asn1.OBJECT_IDENTIFIER) ||
				!bytes.Equal(gotCT, []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0x37, 0x02, 0x01, 0x04}) {
				t.Errorf("contentType attribute % x, want SPC_INDIRECT_DATA", []byte(gotCT))
			}
			set := cryptobyte.NewBuilder(nil)
			set.AddASN1(asn1.SET, func(b *cryptobyte.Builder) { b.AddBytes(p.signedAttrs) })
			attrsDigest := sha256.Sum256(set.BytesOrPanic())
			if err := rsa.VerifyPKCS1v15(h.DB.Cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, attrsDigest[:], p.signature); err != nil {
				t.Errorf("signature does not verify with the db key: %v", err)
			}

			// CheckSum is the imagehlp sum of the signed image with the field zeroed.
			zeroed := bytes.Clone(signed)
			binary.LittleEndian.PutUint32(zeroed[tp.checksumOff:], 0)
			if got, want := binary.LittleEndian.Uint32(signed[tp.checksumOff:]), peChecksum(zeroed); got != want {
				t.Errorf("CheckSum %#x, want %#x", got, want)
			}

			if _, err := SignPE(signed, h.DB); !errors.Is(err, ErrAlreadySigned) {
				t.Errorf("signing a signed image: err %v, want ErrAlreadySigned", err)
			}
		})
	}
}

func TestPEChecksumMatchesImagehlp(t *testing.T) {
	// 16-bit words summed with end-around carry, plus the length:
	// 0xffff + 0x0002 folds to 0x0002, then + 0x0001 (the odd byte) = 0x0003.
	if got := peChecksum([]byte{0xff, 0xff, 0x02, 0x00, 0x01}); got != 0x0003+5 {
		t.Errorf("peChecksum = %#x, want %#x", got, 0x0003+5)
	}
}

func TestSignPERejectsMalformedImages(t *testing.T) {
	h := testHierarchy(t)
	tp := buildTestPE(t, true, 0)

	noDirs := bytes.Clone(tp.image)
	binary.LittleEndian.PutUint32(noDirs[tp.certDirOff-4*8-4:], 4) // NumberOfRvaAndSizes
	truncatedSection := tp.image[:0x300]

	for name, image := range map[string][]byte{
		"empty":                     nil,
		"not MZ":                    append([]byte("XX"), tp.image[2:]...),
		"no Certificate Table":      noDirs,
		"section past end of image": truncatedSection,
	} {
		if _, err := SignPE(image, h.DB); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
