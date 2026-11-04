package secureboot

import (
	encasn1 "encoding/asn1"
	"testing"

	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/cryptobyte/asn1"
)

// parsedSignedData is what the tests read back out of a SignedData.
type parsedSignedData struct {
	contentType encasn1.ObjectIdentifier
	content     []byte // the [0] EXPLICIT content's encoding, nil if detached
	certDER     []byte // the first certificate
	signedAttrs []byte // contents of the [0] IMPLICIT attributes, nil if none
	signature   []byte
}

// parseSignedData reads a bare SignedData, independently of how pkcs7.go
// builds one.
func parseSignedData(t *testing.T, der []byte) parsedSignedData {
	t.Helper()
	var p parsedSignedData
	var sd, digestAlgs, ci, certs, signerInfos, si, issuerSerial, alg cryptobyte.String
	var version int64
	s := cryptobyte.String(der)
	if !s.ReadASN1(&sd, asn1.SEQUENCE) || !s.Empty() {
		t.Fatal("SignedData: not one SEQUENCE")
	}
	if !sd.ReadASN1Integer(&version) || version != 1 {
		t.Fatalf("SignedData version %d, want 1", version)
	}
	if !sd.ReadASN1(&digestAlgs, asn1.SET) {
		t.Fatal("SignedData: no digestAlgorithms")
	}
	if !sd.ReadASN1(&ci, asn1.SEQUENCE) || !ci.ReadASN1ObjectIdentifier(&p.contentType) {
		t.Fatal("SignedData: bad contentInfo")
	}
	if !ci.Empty() {
		var content cryptobyte.String
		if !ci.ReadASN1(&content, tagContext0) {
			t.Fatal("contentInfo: bad [0] content")
		}
		p.content = content
	}
	if !sd.ReadASN1(&certs, tagContext0) {
		t.Fatal("SignedData: no certificates")
	}
	var cert cryptobyte.String
	if !certs.ReadASN1Element(&cert, asn1.SEQUENCE) {
		t.Fatal("certificates: no certificate")
	}
	p.certDER = cert
	if !sd.ReadASN1(&signerInfos, asn1.SET) || !signerInfos.ReadASN1(&si, asn1.SEQUENCE) || !signerInfos.Empty() || !sd.Empty() {
		t.Fatal("SignedData: want exactly one SignerInfo and nothing after it")
	}
	if !si.ReadASN1Integer(&version) || version != 1 ||
		!si.ReadASN1(&issuerSerial, asn1.SEQUENCE) || !si.ReadASN1(&alg, asn1.SEQUENCE) {
		t.Fatal("SignerInfo: bad version, issuerAndSerialNumber or digestAlgorithm")
	}
	if si.PeekASN1Tag(tagContext0) {
		var attrs cryptobyte.String
		si.ReadASN1(&attrs, tagContext0)
		p.signedAttrs = attrs
	}
	var sig cryptobyte.String
	if !si.ReadASN1(&alg, asn1.SEQUENCE) || !si.ReadASN1(&sig, asn1.OCTET_STRING) || !si.Empty() {
		t.Fatal("SignerInfo: bad digestEncryptionAlgorithm or signature")
	}
	p.signature = sig
	return p
}

// signedAttribute returns the single value of attribute oid in attrs.
func signedAttribute(t *testing.T, attrs []byte, oid encasn1.ObjectIdentifier) []byte {
	t.Helper()
	s := cryptobyte.String(attrs)
	for !s.Empty() {
		var attr, values, value cryptobyte.String
		var got encasn1.ObjectIdentifier
		if !s.ReadASN1(&attr, asn1.SEQUENCE) || !attr.ReadASN1ObjectIdentifier(&got) || !attr.ReadASN1(&values, asn1.SET) {
			t.Fatal("bad Attribute")
		}
		if got.Equal(oid) {
			var tag asn1.Tag
			if !values.ReadAnyASN1Element(&value, &tag) || !values.Empty() {
				t.Fatalf("attribute %v: want one value", oid)
			}
			return value
		}
	}
	t.Fatalf("no attribute %v", oid)
	return nil
}
