package secureboot

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf16"

	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"
)

// peImage is the part of a PE/COFF layout the Authenticode hash needs, from
// the Microsoft PE Format specification ("Calculating the PE Image Hash").
type peImage struct {
	checksumOff   int // offset of OptionalHeader.CheckSum
	certDirOff    int // offset of the Certificate Table data directory entry
	sizeOfHeaders int
	certTableOff  int // Certificate Table file offset and size, 0 when unsigned
	certTableSize int
	sections      []peSection
}

type peSection struct{ off, size int }

const (
	pe32Magic     = 0x10b
	pe32PlusMagic = 0x20b
	// certTableIndex is the Certificate Table's index in the data directories.
	certTableIndex = 4
)

func parsePE(b []byte) (*peImage, error) {
	u16 := func(off int) (int, error) {
		if off < 0 || off+2 > len(b) {
			return 0, fmt.Errorf("truncated PE at offset %d", off)
		}
		return int(binary.LittleEndian.Uint16(b[off:])), nil
	}
	u32 := func(off int) (int, error) {
		if off < 0 || off+4 > len(b) {
			return 0, fmt.Errorf("truncated PE at offset %d", off)
		}
		return int(binary.LittleEndian.Uint32(b[off:])), nil
	}

	if len(b) < 0x40 || b[0] != 'M' || b[1] != 'Z' {
		return nil, errors.New("not a PE image: no MZ header")
	}
	peOff, err := u32(0x3c)
	if err != nil {
		return nil, err
	}
	if peOff+4 > len(b) || string(b[peOff:peOff+4]) != "PE\x00\x00" {
		return nil, errors.New("not a PE image: no PE signature")
	}
	coff := peOff + 4
	nSections, err := u16(coff + 2)
	if err != nil {
		return nil, err
	}
	optSize, err := u16(coff + 16)
	if err != nil {
		return nil, err
	}
	opt := coff + 20
	magic, err := u16(opt)
	if err != nil {
		return nil, err
	}
	var numDirsOff, dirsOff int
	switch magic {
	case pe32Magic:
		numDirsOff, dirsOff = opt+92, opt+96
	case pe32PlusMagic:
		numDirsOff, dirsOff = opt+108, opt+112
	default:
		return nil, fmt.Errorf("unknown optional header magic %#x", magic)
	}
	numDirs, err := u32(numDirsOff)
	if err != nil {
		return nil, err
	}
	if numDirs <= certTableIndex {
		return nil, fmt.Errorf("PE image has %d data directories, no Certificate Table entry", numDirs)
	}

	img := &peImage{checksumOff: opt + 64, certDirOff: dirsOff + certTableIndex*8}
	if img.certDirOff+8 > opt+optSize {
		return nil, errors.New("certificate table entry lies outside the optional header")
	}
	if img.sizeOfHeaders, err = u32(opt + 60); err != nil {
		return nil, err
	}
	if img.sizeOfHeaders < img.certDirOff+8 || img.sizeOfHeaders > len(b) {
		return nil, fmt.Errorf("SizeOfHeaders %d out of range", img.sizeOfHeaders)
	}
	if img.certTableOff, err = u32(img.certDirOff); err != nil {
		return nil, err
	}
	if img.certTableSize, err = u32(img.certDirOff + 4); err != nil {
		return nil, err
	}

	secTable := opt + optSize
	for i := range nSections {
		hdr := secTable + i*40
		size, err := u32(hdr + 16)
		if err != nil {
			return nil, err
		}
		off, err := u32(hdr + 20)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			continue
		}
		if off+size > len(b) {
			return nil, fmt.Errorf("section %d (%d bytes at %d) runs past the end of the image", i, size, off)
		}
		img.sections = append(img.sections, peSection{off: off, size: size})
	}
	slices.SortFunc(img.sections, func(a, b peSection) int { return a.off - b.off })
	return img, nil
}

// authenticodeDigest is the SHA-256 Authenticode hash of b: the headers
// without CheckSum and the Certificate Table entry, the sections in file
// order, then whatever follows SUM_OF_BYTES_HASHED short of the certificate
// table. The last step mirrors the spec (and EDK2 and sbsigntool) exactly,
// including its use of a byte count as an offset.
func authenticodeDigest(b []byte, img *peImage) []byte {
	h := sha256.New()
	h.Write(b[:img.checksumOff])
	h.Write(b[img.checksumOff+4 : img.certDirOff])
	h.Write(b[img.certDirOff+8 : img.sizeOfHeaders])
	sum := img.sizeOfHeaders
	for _, s := range img.sections {
		h.Write(b[s.off : s.off+s.size])
		sum += s.size
	}
	if end := len(b) - img.certTableSize; sum < end {
		h.Write(b[sum:end])
	}
	return h.Sum(nil)
}

// peChecksum computes the PE CheckSum field as imagehlp's
// CheckSumMappedFile does; the caller zeroes the field first. Firmware does
// not check it, but sbsign keeps it current and so do we.
func peChecksum(b []byte) uint32 {
	var sum uint32
	add := func(v uint32) {
		sum += v
		sum = (sum & 0xffff) + (sum >> 16)
	}
	for i := 0; i+1 < len(b); i += 2 {
		add(uint32(binary.LittleEndian.Uint16(b[i:])))
	}
	if len(b)%2 == 1 {
		add(uint32(b[len(b)-1]))
	}
	return sum + uint32(len(b)) //nolint:gosec // PE images are limited to 4 GiB
}

// spcIndirectDataContent encodes Authenticode's SpcIndirectDataContent for a
// PE image digest: the SPC_PE_IMAGE_DATA attribute, in the same encoding
// signtool and sbsign emit (no flags, and the "<<<Obsolete>>>" file link),
// then the digest.
func spcIndirectDataContent(digest []byte) ([]byte, error) {
	b := cryptobyte.NewBuilder(nil)
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
			b.AddASN1ObjectIdentifier(oidSPCPEImageData)
			b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) { // SpcPeImageData
				b.AddASN1BitString(nil)                              // flags
				b.AddASN1(tagContext0, func(b *cryptobyte.Builder) { // file
					b.AddASN1(asn1.Tag(2).ContextSpecific().Constructed(), func(b *cryptobyte.Builder) { // SpcLink.file
						b.AddASN1(asn1.Tag(0).ContextSpecific(), func(b *cryptobyte.Builder) { // SpcString.unicode, UTF-16BE
							for _, u := range utf16.Encode([]rune("<<<Obsolete>>>")) {
								b.AddUint16(u)
							}
						})
					})
				})
			})
		})
		b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) { // DigestInfo
			addAlgorithm(b, oidSHA256)
			b.AddASN1OctetString(digest)
		})
	})
	return b.Bytes()
}

// ErrAlreadySigned is returned when SignPE is given an image that already
// carries a Certificate Table.
var ErrAlreadySigned = errors.New("PE image is already signed")

// SignPE Authenticode-signs a PE/EFI image (a UKI, a boot loader) with
// signer, normally the db key, and returns the signed image. The image is
// zero-padded to an 8-byte boundary, the signature is appended as a
// WIN_CERT_TYPE_PKCS_SIGNED_DATA attribute certificate, and the Certificate
// Table entry and CheckSum are updated. An image that is already signed is
// refused with ErrAlreadySigned.
func SignPE(image []byte, signer KeyPair) ([]byte, error) {
	img, err := parsePE(image)
	if err != nil {
		return nil, err
	}
	if img.certTableOff != 0 || img.certTableSize != 0 {
		return nil, ErrAlreadySigned
	}

	out := make([]byte, len(image), len(image)+7+4096)
	copy(out, image)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}

	content, err := spcIndirectDataContent(authenticodeDigest(out, img))
	if err != nil {
		return nil, fmt.Errorf("encode SpcIndirectDataContent: %w", err)
	}
	p7, err := contentSignedData(oidSPCIndirectData, content, signer)
	if err != nil {
		return nil, err
	}

	certOff := len(out)
	length := winCertificateHeaderLength + len(p7)
	out = binary.LittleEndian.AppendUint32(out, uint32(length)) //nolint:gosec // a signature is far below 4 GiB
	out = binary.LittleEndian.AppendUint16(out, winCertRevision)
	out = binary.LittleEndian.AppendUint16(out, winCertTypePKCSSignedData)
	out = append(out, p7...)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	if len(out) > 0xffffffff {
		return nil, errors.New("signed image exceeds 4 GiB")
	}

	binary.LittleEndian.PutUint32(out[img.certDirOff:], uint32(certOff))            //nolint:gosec // bounded by the 4 GiB check above
	binary.LittleEndian.PutUint32(out[img.certDirOff+4:], uint32(len(out)-certOff)) //nolint:gosec // as above
	binary.LittleEndian.PutUint32(out[img.checksumOff:], 0)
	binary.LittleEndian.PutUint32(out[img.checksumOff:], peChecksum(out))
	return out, nil
}
