// Package msuefica embeds Microsoft's third-party UEFI CA certificates, the
// ones hardware option ROMs (GPUs, NICs, storage controllers) and third-party
// boot loaders are signed under. internal/secureboot adds them to the db
// enrollment payload by default (docs/Decisions.md §31, Secure Boot), so a
// machine enrolled with the project's own keys still runs its option ROMs.
//
// The files are byte-for-byte what Microsoft publishes, not code: public
// X.509 certificates in DER form, distributed by Microsoft for exactly this
// use (enrolling them in a UEFI db). Each was fetched from Microsoft's PKI
// repository through the fwlink its "Windows Secure Boot Key Creation and
// Management Guidance" page links, and cross-checked against the copy in
// github.com/microsoft/secureboot_objects (PreSignedObjects/DB/Certificates)
// on 2026-11-04; both sources had the SHA-256 recorded below. The package's
// test fails if an embedded file stops matching it.
//
// Microsoft's KEK CA and Windows Production PCA are deliberately absent: the
// project owns KEK, and §31 names only the UEFI CA in db.
package msuefica

import _ "embed"

// Cert is one embedded Microsoft certificate and its provenance.
type Cert struct {
	// Name is the certificate's Subject CommonName.
	Name string
	// DER is the certificate as Microsoft publishes it.
	DER []byte
	// SourceURL is where the file was fetched from (after the fwlink redirect).
	SourceURL string
	// SHA256 is the hex SHA-256 of DER, as recorded when it was vendored.
	SHA256 string
}

// OwnerGUID is the SignatureOwner Microsoft uses for its entries in db, as
// read from Microsoft's own signed db update
// (secureboot_objects PostSignedObjects/Optional/DB/DBUpdate3P2023.bin).
const OwnerGUID = "77fa9abd-0359-4d32-bd60-28f4e78f784b"

//go:embed MicCorUEFCA2011_2011-06-27.der
var uefiCA2011 []byte

//go:embed MicrosoftUEFICA2023.der
var uefiCA2023 []byte

// UEFICAs returns Microsoft's third-party UEFI CA certificates, 2011 first:
// existing option ROMs chain to the 2011 CA, newer signatures to 2023.
func UEFICAs() []Cert {
	return []Cert{
		{
			Name:      "Microsoft Corporation UEFI CA 2011",
			DER:       uefiCA2011,
			SourceURL: "https://www.microsoft.com/pkiops/certs/MicCorUEFCA2011_2011-06-27.crt (via https://go.microsoft.com/fwlink/p/?linkid=321194)",
			SHA256:    "48e99b991f57fc52f76149599bff0a58c47154229b9f8d603ac40d3500248507",
		},
		{
			Name:      "Microsoft UEFI CA 2023",
			DER:       uefiCA2023,
			SourceURL: "https://www.microsoft.com/pkiops/certs/microsoft%20uefi%20ca%202023.crt (via https://go.microsoft.com/fwlink/?linkid=2239872)",
			SHA256:    "f6124e34125bee3fe6d79a574eaa7b91c0e7bd9d929c1a321178efd611dad901",
		},
	}
}
