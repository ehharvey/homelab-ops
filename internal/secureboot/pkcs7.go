package secureboot

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	encasn1 "encoding/asn1"
	"fmt"
	"slices"

	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"
)

// This file builds the PKCS#7 (RFC 2315) SignedData that both UEFI
// authenticated variables and Authenticode carry: version 1, one signer
// identified by issuer and serial number, SHA-256 digests and RSA PKCS#1 v1.5
// signatures. It only produces signatures; verifying them is firmware's job
// (and sbverify's and openssl's, in the validate script).

var (
	oidData            = encasn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidSignedData      = encasn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidContentType     = encasn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMessageDigest   = encasn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidSHA256          = encasn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidRSAEncryption   = encasn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSPCIndirectData = encasn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 4}
	oidSPCPEImageData  = encasn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 15}
)

var tagContext0 = asn1.Tag(0).ContextSpecific().Constructed()

func addAlgorithm(b *cryptobyte.Builder, oid encasn1.ObjectIdentifier) {
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1ObjectIdentifier(oid)
		b.AddASN1NULL()
	})
}

// signDigest signs a SHA-256 digest with RSA PKCS#1 v1.5, the only
// signature UEFI firmware is required to verify.
func signDigest(signer KeyPair, digest []byte) ([]byte, error) {
	if _, ok := signer.Cert.PublicKey.(*rsa.PublicKey); !ok {
		return nil, fmt.Errorf("signing certificate %q has a %T key, want RSA", signer.Cert.Subject.CommonName, signer.Cert.PublicKey)
	}
	sig, err := signer.Key.Sign(rand.Reader, digest, crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return sig, nil
}

// signedData encodes a SignedData. content is the encoded content of the
// inner ContentInfo's [0] EXPLICIT field, or nil for detached data;
// signedAttrs is the encoded SET OF Attribute's contents, or nil for none.
func signedData(contentType encasn1.ObjectIdentifier, content, signedAttrs, signature []byte, signer KeyPair) ([]byte, error) {
	b := cryptobyte.NewBuilder(nil)
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1Int64(1)
		b.AddASN1(asn1.SET, func(b *cryptobyte.Builder) { addAlgorithm(b, oidSHA256) })
		b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
			b.AddASN1ObjectIdentifier(contentType)
			if content != nil {
				b.AddASN1(tagContext0, func(b *cryptobyte.Builder) { b.AddBytes(content) })
			}
		})
		b.AddASN1(tagContext0, func(b *cryptobyte.Builder) { b.AddBytes(signer.Cert.Raw) })
		b.AddASN1(asn1.SET, func(b *cryptobyte.Builder) {
			b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
				b.AddASN1Int64(1)
				b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
					b.AddBytes(signer.Cert.RawIssuer)
					b.AddASN1BigInt(signer.Cert.SerialNumber)
				})
				addAlgorithm(b, oidSHA256)
				if signedAttrs != nil {
					b.AddASN1(tagContext0, func(b *cryptobyte.Builder) { b.AddBytes(signedAttrs) })
				}
				addAlgorithm(b, oidRSAEncryption)
				b.AddASN1OctetString(signature)
			})
		})
	})
	der, err := b.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode SignedData: %w", err)
	}
	return der, nil
}

// detachedSignedData returns a bare SignedData (not wrapped in a
// ContentInfo) with no signed attributes, whose signature is over digest
// itself. This is what EFI_VARIABLE_AUTHENTICATION_2's CertData holds: EDK2
// reads the digest algorithm at a fixed offset into it, which only holds for
// a bare SignedData.
func detachedSignedData(digest []byte, signer KeyPair) ([]byte, error) {
	sig, err := signDigest(signer, digest)
	if err != nil {
		return nil, err
	}
	return signedData(oidData, nil, nil, sig, signer)
}

// attribute encodes one Attribute: SEQUENCE { type, SET { value } }, where
// value is already DER.
func attribute(oid encasn1.ObjectIdentifier, value []byte) ([]byte, error) {
	b := cryptobyte.NewBuilder(nil)
	b.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1ObjectIdentifier(oid)
		b.AddASN1(asn1.SET, func(b *cryptobyte.Builder) { b.AddBytes(value) })
	})
	return b.Bytes()
}

// contentSignedData returns a ContentInfo-wrapped SignedData carrying
// content (DER of the inner content value) under contentType, with the
// contentType and messageDigest signed attributes. The messageDigest is over
// the content's value octets, without its tag and length (RFC 2315 §9.3),
// which is what Authenticode verifiers recompute.
func contentSignedData(contentType encasn1.ObjectIdentifier, content []byte, signer KeyPair) ([]byte, error) {
	var inner cryptobyte.String
	var tag asn1.Tag
	s := cryptobyte.String(content)
	if !s.ReadAnyASN1(&inner, &tag) || !s.Empty() {
		return nil, fmt.Errorf("content is not a single DER element")
	}
	contentDigest := sha256.Sum256(inner)

	ctOID := cryptobyte.NewBuilder(nil)
	ctOID.AddASN1ObjectIdentifier(contentType)
	ctValue, err := ctOID.Bytes()
	if err != nil {
		return nil, err
	}
	mdValue := cryptobyte.NewBuilder(nil)
	mdValue.AddASN1OctetString(contentDigest[:])
	mdBytes, err := mdValue.Bytes()
	if err != nil {
		return nil, err
	}

	ctAttr, err := attribute(oidContentType, ctValue)
	if err != nil {
		return nil, err
	}
	mdAttr, err := attribute(oidMessageDigest, mdBytes)
	if err != nil {
		return nil, err
	}
	// DER orders a SET OF by its elements' encodings.
	attrs := [][]byte{ctAttr, mdAttr}
	slices.SortFunc(attrs, bytes.Compare)
	attrsContent := bytes.Join(attrs, nil)

	// The signature is over the attributes encoded as a SET (tag 0x31), not
	// the [0] IMPLICIT tag they are stored under.
	set := cryptobyte.NewBuilder(nil)
	set.AddASN1(asn1.SET, func(b *cryptobyte.Builder) { b.AddBytes(attrsContent) })
	setDER, err := set.Bytes()
	if err != nil {
		return nil, err
	}
	attrsDigest := sha256.Sum256(setDER)
	sig, err := signDigest(signer, attrsDigest[:])
	if err != nil {
		return nil, err
	}

	sd, err := signedData(contentType, content, attrsContent, sig, signer)
	if err != nil {
		return nil, err
	}

	ci := cryptobyte.NewBuilder(nil)
	ci.AddASN1(asn1.SEQUENCE, func(b *cryptobyte.Builder) {
		b.AddASN1ObjectIdentifier(oidSignedData)
		b.AddASN1(tagContext0, func(b *cryptobyte.Builder) { b.AddBytes(sd) })
	})
	return ci.Bytes()
}
