#! /bin/bash
# Fetches the pinned IncusOS base image from the Linux Containers CDN, verifies
# it, and caches it where every worktree's validate scripts find it (#296).
#
#   fetch-incusos-base.sh                  fetch the pinned version (make incusos-base)
#   fetch-incusos-base.sh --latest-stable  print the newest `stable` version on the CDN
#   fetch-incusos-base.sh --path           print where the image is cached; no network
#
# Why this exists: INCUSOS_BASE_IMAGE used to be a 3.2 GB file placed by hand,
# with no recorded origin, so every [base-image] check skipped on any host
# where nobody had fetched one (#268 changed a line those checks cover and
# still couldn't run them). flasher-tool's own download can't fill the gap
# (#206).
#
# The pin is scripts/incusos-base.version: one line, a CDN `stable` version.
# It is NOT the third_party/incus-os submodule tag, because not every tag gets
# a CDN build and old builds are pruned. INCUSOS_VERSION overrides the pin for
# this script only; the validate harness defaults to the pinned version alone
# (scripts/validate/lib.sh, default_incusos_base_image).
#
# Trust chain. The CDN publishes no .sha256 file; the checksum lives in the
# S/MIME-signed <version>/update.sjson. So:
#
#   scripts/incusos-keys/root-E1.crt   committed, never fetched at run time:
#                                      taking the trust root from the CDN being
#                                      verified would be circular
#     -> update.sjson's signature, by a certificate issued by the
#        "Incus OS - Update E1" CA, itself issued directly by Root E1
#     -> the .img.gz's sha256, read from that verified JSON
#     -> the decompressed .img's sha256, written to a sidecar and re-checked
#        on every later run (local corruption, not authenticity)
#
# `openssl smime -verify -CAfile root-E1.crt` alone accepts a signer under ANY
# Root E1 intermediate. Upstream trusts only the Update CA (incus-osd
# internal/providers/provider_images.go: util.VerifySMIME with
# UpdateCACertificate), so the chain's shape is checked too. The signing
# certificates rotate yearly ("Incus OS - Update 2025 E1", "... 2026 E1"), which
# is why the check is on their issuer rather than on their own subject. It
# stops there: the image's own contents (sysext/verity signatures) are not
# verified.
#
# Cache: <main checkout>/bootstrap-output/incusos/<version>/, gitignored and
# shared by every worktree. Each issue gets its own worktree with an empty
# bootstrap-output/ (#252), so a per-checkout cache would leave every worker
# skipping. A fetch is assembled in a temp directory beside the cache and
# renamed into place as a whole, so an interrupted run leaves nothing at the
# final path. A cached image that no longer matches its sidecar is an error
# naming the directory to delete, never a silent re-fetch.
#
# stdout carries only the result (the `export` line, the version or the path);
# progress and errors go to stderr.
#
# Test seams, each announced on stderr when set. None of them skips a
# verification step; they only change what is verified against, and where the
# result lands:
#   INCUSOS_CDN_URL    base URL instead of https://images.linuxcontainers.org/os
#                      (curl accepts file://)
#   INCUSOS_ROOT_CERT  trust root instead of the committed root-E1.crt
#   INCUSOS_CACHE_DIR  cache directory instead of the main checkout's
#
# Needs curl, openssl, jq, gzip and sha256sum.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
pin_file="$repo/scripts/incusos-base.version"
root_cert="${INCUSOS_ROOT_CERT:-$repo/scripts/incusos-keys/root-E1.crt}"
cdn="${INCUSOS_CDN_URL:-https://images.linuxcontainers.org/os}"
cdn="${cdn%/}"

# The CA the signer of every .sjson must be issued by, in RFC 2253 form.
update_ca="O=Linux Containers,CN=Incus OS - Update E1"

say() { echo "$*" >&2; }
die() {
	say "ERROR: $*"
	exit 1
}

usage() {
	sed -n '5,7p' "$0" | sed 's/^# \{0,3\}//' >&2
	exit 2
}

# The one place the shared cache is resolved; lib.sh reaches it through --path.
# The main checkout is found as scripts/worktree.sh finds it; in a plain clone
# or on a CI runner that is the checkout itself.
cache_dir() {
	if [ -n "${INCUSOS_CACHE_DIR:-}" ]; then
		echo "${INCUSOS_CACHE_DIR%/}"
		return
	fi
	local common
	if common=$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir 2>/dev/null); then
		echo "$(dirname "$common")/bootstrap-output/incusos"
	else
		echo "$repo/bootstrap-output/incusos"
	fi
}

# INCUSOS_VERSION, else the pin file. Always a 12-digit incus-os build stamp:
# it becomes part of a URL and a path.
resolve_version() {
	local v="${INCUSOS_VERSION:-}"
	if [ -z "$v" ]; then
		[ -f "$pin_file" ] || die "$pin_file not found"
		v=$(tr -d '[:space:]' <"$pin_file")
	fi
	[[ "$v" =~ ^[0-9]{12}$ ]] || die "'$v' is not an IncusOS version (12 digits, e.g. 202610041640)"
	echo "$v"
}

need() {
	local c
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 || die "$c is required but not installed"
	done
}

note_seams() {
	[ -z "${INCUSOS_CDN_URL:-}" ] || say "NOTE: INCUSOS_CDN_URL set: fetching from $cdn, not the Linux Containers CDN"
	[ -z "${INCUSOS_ROOT_CERT:-}" ] || say "NOTE: INCUSOS_ROOT_CERT set: trusting $root_cert, not the committed root-E1.crt"
	[ -z "${INCUSOS_CACHE_DIR:-}" ] || say "NOTE: INCUSOS_CACHE_DIR set: caching in ${INCUSOS_CACHE_DIR%/}, not the main checkout"
}

download() {
	local url="$1" out="$2" progress="-sS"
	# The image is ~610 MB; show a bar when someone is watching.
	if [ "${3:-}" = big ] && [ -t 2 ]; then
		progress="--progress-bar"
	fi
	curl -fL --retry 3 "$progress" -o "$out" "$url" || die "download failed: $url"
}

# verify_sjson <in.sjson> <out.json> — writes the signed JSON to out.json only
# if the signature verifies against the trust root AND the signer was issued by
# the Update CA. -no-CApath/-no-CAstore keep the system CA store out of it:
# without them openssl trusts that store alongside -CAfile.
verify_sjson() {
	local sjson="$1" out="$2"
	local signer="$work/signer.pem" bundle="$work/bundle.pem" body="$work/body.json" err="$work/openssl.err"
	rm -f "$signer" "$bundle" "$body" "$out"

	[ -f "$root_cert" ] || die "trust root $root_cert not found"

	# -text strips the `Content-Type: text/plain` MIME header, and refuses any
	# other content type.
	openssl smime -verify -text -in "$sjson" \
		-CAfile "$root_cert" -no-CApath -no-CAstore \
		-signer "$signer" -out "$body" 2>"$err" ||
		die "signature verification failed for $(basename "$sjson"): $(grep -v '^Verification' "$err" | tail -n 1)"

	[ "$(grep -c 'BEGIN CERTIFICATE' "$signer")" = 1 ] ||
		die "$(basename "$sjson") must have exactly one signer"

	# The chain must be exactly signer <- Update CA <- trust root.
	openssl smime -pk7out -in "$sjson" 2>"$err" | openssl pkcs7 -print_certs -out "$bundle" 2>>"$err" ||
		die "could not read the certificates in $(basename "$sjson")"
	local chain
	chain=$(openssl verify -CAfile "$root_cert" -no-CApath -no-CAstore \
		-untrusted "$bundle" -show_chain -nameopt RFC2253 "$signer" 2>"$err") ||
		die "signer of $(basename "$sjson") does not chain to the trust root: $(tail -n 1 "$err")"
	chain=$(grep '^depth=' <<<"$chain" | sed 's/ (untrusted)$//')
	if [ "$(wc -l <<<"$chain")" != 3 ] || [ "$(sed -n 2p <<<"$chain")" != "depth=1: $update_ca" ]; then
		say "ERROR: $(basename "$sjson") is validly signed, but not by a certificate issued by the Update CA."
		say "       Wanted the signer's issuer to be '$update_ca', directly under the trust root; got:"
		sed 's/^/         /' <<<"$chain" >&2
		exit 1
	fi

	jq -e . "$body" >/dev/null 2>&1 || die "$(basename "$sjson") verified but does not contain JSON"
	mv "$body" "$out"
}

# check_cached <version> — 0 if the cached pair is intact (prints nothing),
# 1 if there is nothing cached yet; exits non-zero on anything in between.
check_cached() {
	local v="$1" dir img
	dir="$(cache_dir)/$v"
	img="IncusOS_$v.img"
	if [ ! -e "$dir" ] || { [ -d "$dir" ] && [ -z "$(ls -A "$dir")" ]; }; then
		return 1
	fi
	if [ -f "$dir/$img" ] && [ -f "$dir/$img.sha256" ]; then
		say "Checking cached IncusOS $v against its sidecar ..."
		if (cd "$dir" && sha256sum --status -c "$img.sha256"); then
			return 0
		fi
		say "ERROR: $dir/$img no longer matches $img.sha256."
	else
		say "ERROR: $dir exists but does not hold both $img and $img.sha256."
	fi
	say "       Nothing was fetched. Delete the directory and run 'make incusos-base' again:"
	say "         rm -rf $(printf '%q' "$dir")"
	exit 1
}

print_export() {
	printf 'export INCUSOS_BASE_IMAGE=%q\n' "$(cache_dir)/$1/IncusOS_$1.img"
}

latest_stable() {
	need curl openssl jq
	note_seams
	work=$(mktemp -d)
	trap 'rm -rf "$work"' EXIT
	download "$cdn/index.sjson" "$work/index.sjson"
	verify_sjson "$work/index.sjson" "$work/index.json"
	local v
	v=$(jq -r '[.updates[] | select((.channels // []) | index("stable")) | .version
		| select(test("^[0-9]{12}$"))] | max // empty' "$work/index.json")
	[ -n "$v" ] || die "the verified index lists no stable version"
	echo "$v"
}

fetch() {
	need curl openssl jq gzip sha256sum
	note_seams
	local v cache dir img gz want got
	v=$(resolve_version)
	cache=$(cache_dir)
	dir="$cache/$v"
	img="IncusOS_$v.img"
	gz="x86_64/$img.gz"

	if check_cached "$v"; then
		say "IncusOS $v is already cached; nothing to download."
		print_export "$v"
		return
	fi

	mkdir -p "$cache"
	# Beside the cache, so the final rename stays on one filesystem.
	work=$(mktemp -d "$cache/.fetch.XXXXXX")
	trap 'rm -rf "$work"' EXIT

	say "Fetching IncusOS $v from $cdn ..."
	download "$cdn/$v/update.sjson" "$work/update.sjson"
	verify_sjson "$work/update.sjson" "$work/update.json"
	say "update.sjson verified (signer issued by '$update_ca')."

	# The filename carries the version, so a validly signed manifest for some
	# other version can't stand in for this one.
	want=$(jq -r --arg f "$gz" '[.files[] | select(.filename == $f) | .sha256] | first // empty' "$work/update.json")
	[[ "$want" =~ ^[0-9a-f]{64}$ ]] || die "the verified update.sjson has no sha256 for $gz"

	download "$cdn/$v/$gz" "$work/$img.gz" big
	got=$(sha256sum "$work/$img.gz" | cut -d' ' -f1)
	[ "$got" = "$want" ] || die "sha256 mismatch for $gz: the signed manifest says $want, the download is $got"
	say "$img.gz matches the signed sha256."

	mkdir "$work/out"
	gzip -dc "$work/$img.gz" >"$work/out/$img" || die "could not decompress $img.gz"
	rm -f "$work/$img.gz"
	(cd "$work/out" && sha256sum "$img" >"$img.sha256")

	# Image and sidecar appear together or not at all. -T makes this a plain
	# rename, which fails rather than nesting if another run got there first.
	if ! mv -T "$work/out" "$dir" 2>/dev/null; then
		check_cached "$v" || die "could not move the fetched image into $dir"
		say "Another run cached IncusOS $v first; using that copy."
	else
		say "Cached IncusOS $v in $dir"
	fi
	print_export "$v"
}

case "${1:-}" in
"")
	fetch
	;;
--latest-stable)
	[ $# -eq 1 ] || usage
	latest_stable
	;;
--path)
	[ $# -eq 1 ] || usage
	v=$(resolve_version)
	echo "$(cache_dir)/$v/IncusOS_$v.img"
	;;
*)
	usage
	;;
esac
