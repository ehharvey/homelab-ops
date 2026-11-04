// Package msdbx embeds Microsoft's x64 Secure Boot forbidden-signature
// database (dbx). internal/secureboot re-signs its signature lists with the
// project's KEK and enrolls them as dbx whenever db trusts Microsoft's UEFI
// CA (docs/Decisions.md §31, Secure Boot). That CA has signed shims and boot
// loaders that were later revoked, and clearing a machine's factory keys to
// reach Setup Mode can empty its dbx too, after which those binaries would
// boot again.
//
// dbx_x64.efiauth2 is byte-for-byte the file Microsoft publishes as its
// recommended x64 dbx: an EFI_VARIABLE_AUTHENTICATION_2 update signed by
// Microsoft's KEK 2023, at github.com/microsoft/secureboot_objects
// PostSignedObjects/SignedByKEK2023/dbx_x64.efiauth2. It was fetched at
// commit 3d762aecbaecd12402efd21ba89c8c7bd40da4bd on 2026-11-04. The file
// last changed in 92b433ecf7154c7bf1b8f45245233bdde6cd14e5 (2026-09-01), and
// its git blob is the same at both commits. Microsoft's signature is no use
// here, because the project owns KEK, so only the signature lists after the
// authentication descriptor are enrolled.
//
// It holds 291 SHA-256 Authenticode hashes, all owned by Microsoft. Microsoft
// leaves out the 154 x64 entries that its PreSignedObjects/DBX/
// dbx_info_msft_latest.json marks isOptional. 151 of those are Windows boot
// managers under the Windows Production PCA 2011, which the project's db
// never trusts; the other 3 are 2020 Canonical GRUB builds.
//
// To refresh it, fetch the file at a newer commit, then update this comment,
// X64SHA256 and X64Hashes, and re-run make test and
// scripts/validate/secureboot-keys-sign-and-verify-a-test-binary.sh.
// systemd-boot replaces dbx rather than appending to it, so an out-of-date
// copy here also replaces a newer dbx the firmware already had.
package msdbx

import _ "embed"

const (
	// X64SourceURL is where dbx_x64.efiauth2 was fetched from.
	X64SourceURL = "https://raw.githubusercontent.com/microsoft/secureboot_objects/3d762aecbaecd12402efd21ba89c8c7bd40da4bd/PostSignedObjects/SignedByKEK2023/dbx_x64.efiauth2"
	// X64SHA256 is the hex SHA-256 of dbx_x64.efiauth2, as recorded when it
	// was vendored.
	X64SHA256 = "524c796ade42db0048b656f6cf3863832174ae7f58cad6986d1ccae1e3d3ecd1"
	// X64Hashes is the number of SHA-256 entries in its signature lists.
	X64Hashes = 291
)

//go:embed dbx_x64.efiauth2
var x64 []byte

// X64 returns Microsoft's x64 dbx update as published: an
// EFI_VARIABLE_AUTHENTICATION_2 descriptor, then the signature lists.
func X64() []byte { return x64 }
