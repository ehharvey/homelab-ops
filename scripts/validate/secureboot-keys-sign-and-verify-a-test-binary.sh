#! /bin/bash
# shellcheck disable=SC2329 # the helpers below run through check and fails, which shellcheck cannot follow
# Validates GH issue #489 ("Add internal/secureboot: project PK/KEK/db,
# Microsoft UEFI CA by default, and EFI signing") "Done when": the real
# bootstrap CLI generates PK/KEK/db, builds the PK/KEK/db enrollment payloads
# (db with Microsoft's UEFI CA 2011 and 2023 by default, without them under
# --no-microsoft-ca), and Authenticode-signs an EFI binary, and tools that
# share no code with internal/secureboot accept all of it. With the Microsoft
# CA it also builds dbx from Microsoft's x64 revocations, re-signed by KEK
# (the #503 review: an empty dbx would let revoked Microsoft-signed shims
# boot), and a --force run without the CA removes that dbx:
#
#   - openssl checks each key matches its cert, the PK -> KEK -> db chain, the
#     .auth payloads' PKCS#7 signatures, and the Authenticode signer's chain;
#   - efitools' sig-list-to-certs reads the signature lists back, and its
#     sign-efi-sig-list, given the same key and timestamp, reproduces each
#     .auth byte for byte (RSA PKCS#1 v1.5 signatures are deterministic);
#   - sbsigntool's sbverify accepts the signed binary with db.crt, and only
#     with this hierarchy's db.crt.
#
# The test binary is efitools' HelloWorld.efi (or VALIDATE_TEST_EFI). sbverify
# and efitools are optional: without them their checks SKIP (exit 3), tagged
# secureboot-tools. The devcontainer installs both.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="the bootstrap CLI's Secure Boot keys, enrollment payloads (dbx included) and signed EFI binary pass openssl, efitools and sbverify (#489)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="go openssl od cmp sha256sum [sbverify] [sbattach] [sig-list-to-certs] [sign-efi-sig-list]"
VALIDATE_DURATION="~5s"
VALIDATE_SUPPORTED_OS="any"

validate_parse_args "$@"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

BOOTSTRAP_BIN="$WORK_DIR/bootstrap"
MS="$WORK_DIR/with-ms"  # the default: db includes Microsoft's UEFI CA
NOMS="$WORK_DIR/no-ms"  # --no-microsoft-ca

MS_2011_SHA256=48e99b991f57fc52f76149599bff0a58c47154229b9f8d603ac40d3500248507
MS_2023_SHA256=f6124e34125bee3fe6d79a574eaa7b91c0e7bd9d929c1a321178efd611dad901
MS_OWNER_GUID=77fa9abd-0359-4d32-bd60-28f4e78f784b
MS_DBX="$ROOT_DIR/internal/third_party/msdbx/dbx_x64.efiauth2"
# EFI_GLOBAL_VARIABLE and EFI_IMAGE_SECURITY_DATABASE_GUID in wire byte order.
GLOBAL_GUID='\x61\xdf\xe4\x8b\xca\x93\xd2\x11\xaa\x0d\x00\xe0\x98\x03\x2b\x8c'
DB_GUID='\xcb\xb2\x19\xd7\x3a\x3d\x96\x45\xa3\xbc\xda\xd0\x0e\x67\x65\x6f'

fails() { ! "$@"; }

# hex16 N: N as two big-endian \x escapes, for printf '%b'.
hex16() { printf '\\x%02x\\x%02x' $(($1 >> 8)) $(($1 & 255)); }

# auth_len FILE: the .auth's WIN_CERTIFICATE dwLength (little-endian host).
auth_len() { od -A n -t u4 -j 16 -N 4 "$1" | tr -d ' '; }

# auth_contentinfo AUTH: the .auth's bare PKCS#7 SignedData, wrapped in the
# ContentInfo openssl expects. head reads only the descriptor and tail drains
# all of it: the other way round, head exits before tail has written a large
# value (dbx's), and under pipefail tail's SIGPIPE fails the check.
auth_contentinfo() {
  local n
  n=$(($(auth_len "$1") - 24))
  printf '%b' "\\x30\\x82$(hex16 $((15 + n)))\\x06\\x09\\x2a\\x86\\x48\\x86\\xf7\\x0d\\x01\\x07\\x02\\xa0\\x82$(hex16 "$n")"
  head -c $((40 + n)) "$1" | tail -c "$n"
}

# signed_bundle NAME GUID AUTH ESL: what a time-based authenticated variable
# signs: the name in UTF-16LE, the vendor GUID, the attributes
# (NV|BS|RT|TIME_BASED_AUTHENTICATED_WRITE_ACCESS), the .auth's timestamp,
# then the value.
signed_bundle() {
  local name="$1" guid="$2" auth="$3" esl="$4" i
  for ((i = 0; i < ${#name}; i++)); do printf '%s\0' "${name:i:1}"; done
  printf '%b' "$guid"
  printf '\x27\x00\x00\x00'
  head -c 16 "$auth"
  cat "$esl"
}

# verify_auth NAME GUID DIR CAFILE: openssl verifies DIR/NAME.auth's
# signature over its bundle and the signer's chain to CAFILE.
verify_auth() {
  local name="$1" guid="$2" dir="$3" ca="$4" tmp="$WORK_DIR/verify-$RANDOM"
  auth_contentinfo "$dir/$name.auth" >"$tmp.p7" &&
    signed_bundle "$name" "$guid" "$dir/$name.auth" "$dir/$name.esl" >"$tmp.bundle" &&
    openssl smime -verify -binary -inform DER -in "$tmp.p7" -content "$tmp.bundle" \
      -CAfile "$ca" -purpose any -out /dev/null
}

# auth_value_is AUTH ESL: what follows AUTH's descriptor is ESL, byte for byte.
auth_value_is() {
  tail -c +$((16 + $(auth_len "$1") + 1)) "$1" | cmp -s - "$2"
}

# auth_carries_esl AUTH ESL: the .auth ends with the signature list it signs.
auth_carries_esl() {
  tail -c "$(stat -c %s "$2")" "$1" | cmp -s - "$2"
}

# auth_timestamp AUTH: the EFI_TIME as sign-efi-sig-list's -t wants it.
auth_timestamp() {
  local b
  read -r -a b < <(od -A n -t u1 -N 7 "$1")
  printf '%d-%d-%d %d:%d:%d' $((b[0] + 256 * b[1])) "${b[2]}" "${b[3]}" "${b[4]}" "${b[5]}" "${b[6]}"
}

# efitools_reproduces_auth NAME DIR OFFLINE SIGNER: sign-efi-sig-list signs
# DIR/NAME.esl with SIGNER's key at the .auth's timestamp and gets the same bytes.
efitools_reproduces_auth() {
  local name="$1" dir="$2" keydir="$3" signer="$4" out="$WORK_DIR/efitools-$RANDOM.auth"
  sign-efi-sig-list -t "$(auth_timestamp "$dir/$name.auth")" -k "$keydir/$signer.key" -c "$dir/$signer.crt" \
    "$name" "$dir/$name.esl" "$out" >/dev/null &&
    cmp -s "$out" "$dir/$name.auth"
}

# esl_split ESL PREFIX: sig-list-to-certs writes PREFIX-<n>.der and logs each
# entry's owner GUID to PREFIX.log.
esl_split() { sig-list-to-certs "$1" "$2" >"$2.log" 2>&1; }
esl_count() { find "$(dirname "$1")" -name "$(basename "$1")-*.der" | wc -l; }
esl_owners() { sed -n 's/^file .*: Guid \(.*\)$/\1/p' "$1.log" | tr '\n' ' ' | sed 's/ $//'; }
same_cert() { openssl x509 -in "$1" -outform DER | cmp -s - "$2"; }
sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
key_matches_cert() { [ "$(openssl x509 -noout -modulus -in "$2")" = "$(openssl rsa -noout -modulus -in "$1" 2>/dev/null)" ]; }
is_rsa2048_sha256() {
  openssl x509 -noout -text -in "$1" | grep -q 'Public-Key: (2048 bit)' &&
    openssl x509 -noout -text -in "$1" | grep -q 'Signature Algorithm: sha256WithRSAEncryption'
}

echo "== 0. Hard prerequisites =="
require_cmd go openssl od cmp sha256sum stat find
check_prereqs

echo
echo "== 1. Build the bootstrap CLI =="
check "bootstrap CLI builds" go -C "$ROOT_DIR" build -o "$BOOTSTRAP_BIN" ./cmd/bootstrap
if [ ! -x "$BOOTSTRAP_BIN" ]; then
  echo "ERROR: bootstrap CLI didn't build; nothing downstream can be meaningful" >&2
  summary
fi

echo
echo "== 2. gen-secureboot-keys writes the hierarchy, PK/KEK keys apart =="
check "gen-secureboot-keys exits 0" \
  "$BOOTSTRAP_BIN" gen-secureboot-keys --output-dir "$MS/out" --offline-dir "$MS/offline"
check "gen-secureboot-keys --no-microsoft-ca exits 0" \
  "$BOOTSTRAP_BIN" gen-secureboot-keys --no-microsoft-ca --output-dir "$NOMS/out" --offline-dir "$NOMS/offline"
check "a second run without --force refuses to overwrite" \
  fails "$BOOTSTRAP_BIN" gen-secureboot-keys --output-dir "$MS/out" --offline-dir "$MS/offline"
check_eq "the offline dir holds only the PK and KEK keys" "KEK.key PK.key" \
  "$(find "$MS/offline" -mindepth 1 -printf '%f\n' | sort | tr '\n' ' ' | sed 's/ $//')"
check "the output dir holds no PK or KEK key" test ! -e "$MS/out/PK.key" -a ! -e "$MS/out/KEK.key"
check "with-ms: dbx.esl and dbx.auth are written" test -s "$MS/out/dbx.esl" -a -s "$MS/out/dbx.auth"
check "no-ms: no dbx is written" test ! -e "$NOMS/out/dbx.esl" -a ! -e "$NOMS/out/dbx.auth"
cp -a "$MS" "$WORK_DIR/reforce"
check "--force --no-microsoft-ca over a with-ms set exits 0" \
  "$BOOTSTRAP_BIN" gen-secureboot-keys --force --no-microsoft-ca \
  --output-dir "$WORK_DIR/reforce/out" --offline-dir "$WORK_DIR/reforce/offline"
check "... and removes the earlier run's dbx" \
  test ! -e "$WORK_DIR/reforce/out/dbx.esl" -a ! -e "$WORK_DIR/reforce/out/dbx.auth"
check_eq "PK.key is mode 600" 600 "$(stat -c %a "$MS/offline/PK.key" 2>/dev/null)"
check_eq "KEK.key is mode 600" 600 "$(stat -c %a "$MS/offline/KEK.key" 2>/dev/null)"
check_eq "db.key is mode 600" 600 "$(stat -c %a "$MS/out/db.key" 2>/dev/null)"

echo
echo "== 3. openssl: keys, algorithms and the PK -> KEK -> db chain =="
check "PK.key matches PK.crt" key_matches_cert "$MS/offline/PK.key" "$MS/out/PK.crt"
check "KEK.key matches KEK.crt" key_matches_cert "$MS/offline/KEK.key" "$MS/out/KEK.crt"
check "db.key matches db.crt" key_matches_cert "$MS/out/db.key" "$MS/out/db.crt"
for c in PK KEK db; do
  check "$c.crt is RSA-2048, sha256WithRSAEncryption" is_rsa2048_sha256 "$MS/out/$c.crt"
done
check "PK.crt is self-signed" openssl verify -CAfile "$MS/out/PK.crt" "$MS/out/PK.crt"
check "KEK.crt verifies under PK.crt" openssl verify -CAfile "$MS/out/PK.crt" "$MS/out/KEK.crt"
check "db.crt verifies under PK.crt through KEK.crt" \
  openssl verify -CAfile "$MS/out/PK.crt" -untrusted "$MS/out/KEK.crt" "$MS/out/db.crt"
check "db.crt does not verify under the other hierarchy's PK" \
  fails openssl verify -CAfile "$NOMS/out/PK.crt" -untrusted "$NOMS/out/KEK.crt" "$MS/out/db.crt"

echo
echo "== 4. openssl: each .auth is signed up the hierarchy =="
for d in "$MS" "$NOMS"; do
  v=$(basename "$d")
  check "$v: PK.auth verifies, signed by PK" verify_auth PK "$GLOBAL_GUID" "$d/out" "$d/out/PK.crt"
  check "$v: KEK.auth verifies, signed by PK" verify_auth KEK "$GLOBAL_GUID" "$d/out" "$d/out/PK.crt"
  check "$v: db.auth verifies, signed by KEK under PK" verify_auth db "$DB_GUID" "$d/out" "$d/out/PK.crt"
  for n in PK KEK db; do
    check "$v: $n.auth carries $n.esl" auth_carries_esl "$d/out/$n.auth" "$d/out/$n.esl"
  done
done
check "with-ms: dbx.auth verifies, signed by KEK under PK" verify_auth dbx "$DB_GUID" "$MS/out" "$MS/out/PK.crt"
check "with-ms: dbx.auth carries dbx.esl" auth_carries_esl "$MS/out/dbx.auth" "$MS/out/dbx.esl"
check "with-ms: dbx.esl is Microsoft's x64 dbx after its descriptor, byte for byte" \
  auth_value_is "$MS_DBX" "$MS/out/dbx.esl"
check "db.auth does not verify under the other hierarchy's PK" \
  fails verify_auth db "$DB_GUID" "$MS/out" "$NOMS/out/PK.crt"
check "db.auth does not verify as a different variable" \
  fails verify_auth db "$GLOBAL_GUID" "$MS/out" "$MS/out/PK.crt"

echo
echo "== 5. efitools: signature list contents =="
if ! have_cmd sig-list-to-certs; then
  for c in "with-ms: db.esl holds db.crt, then Microsoft UEFI CA 2011 and 2023" \
    "no-ms: db.esl holds db.crt alone" "PK.esl and KEK.esl hold PK.crt and KEK.crt alone"; do
    skip_check "$c" secureboot-tools "sig-list-to-certs not installed (efitools)"
  done
else
  mkdir -p "$WORK_DIR/esl"
  for d in "$MS" "$NOMS"; do
    v=$(basename "$d")
    for n in PK KEK db; do esl_split "$d/out/$n.esl" "$WORK_DIR/esl/$v-$n"; done
  done
  guid=$(cat "$MS/out/GUID")
  check_eq "with-ms: db.esl has 3 entries" 3 "$(esl_count "$WORK_DIR/esl/with-ms-db")"
  check "with-ms: db entry 0 is db.crt" same_cert "$MS/out/db.crt" "$WORK_DIR/esl/with-ms-db-0.der"
  check_eq "with-ms: db entry 1 is Microsoft Corporation UEFI CA 2011" "$MS_2011_SHA256" \
    "$(sha256_of "$WORK_DIR/esl/with-ms-db-1.der" 2>/dev/null)"
  check_eq "with-ms: db entry 2 is Microsoft UEFI CA 2023" "$MS_2023_SHA256" \
    "$(sha256_of "$WORK_DIR/esl/with-ms-db-2.der" 2>/dev/null)"
  check_eq "with-ms: db owners are ours, then Microsoft's" "$guid $MS_OWNER_GUID $MS_OWNER_GUID" \
    "$(esl_owners "$WORK_DIR/esl/with-ms-db")"
  check_eq "no-ms: db.esl has 1 entry" 1 "$(esl_count "$WORK_DIR/esl/no-ms-db")"
  check "no-ms: db entry 0 is db.crt" same_cert "$NOMS/out/db.crt" "$WORK_DIR/esl/no-ms-db-0.der"
  check_eq "no-ms: db owner is ours" "$(cat "$NOMS/out/GUID")" "$(esl_owners "$WORK_DIR/esl/no-ms-db")"
  for n in PK KEK; do
    check_eq "with-ms: $n.esl has 1 entry (no Microsoft KEK)" 1 "$(esl_count "$WORK_DIR/esl/with-ms-$n")"
    check "with-ms: $n entry 0 is $n.crt" same_cert "$MS/out/$n.crt" "$WORK_DIR/esl/with-ms-$n-0.der"
    check_eq "with-ms: $n owner is ours" "$guid" "$(esl_owners "$WORK_DIR/esl/with-ms-$n")"
  done
fi

echo
echo "== 6. efitools: sign-efi-sig-list reproduces each .auth =="
if ! have_cmd sign-efi-sig-list; then
  skip_check "sign-efi-sig-list reproduces PK.auth, KEK.auth, db.auth and dbx.auth" secureboot-tools \
    "sign-efi-sig-list not installed (efitools)"
else
  for d in "$MS" "$NOMS"; do
    v=$(basename "$d")
    check "$v: PK.auth is byte-identical to efitools' (signed by PK)" efitools_reproduces_auth PK "$d/out" "$d/offline" PK
    check "$v: KEK.auth is byte-identical to efitools' (signed by PK)" efitools_reproduces_auth KEK "$d/out" "$d/offline" PK
    # db is signed by KEK, whose key is in the offline dir and cert in out.
    check "$v: db.auth is byte-identical to efitools' (signed by KEK)" efitools_reproduces_auth db "$d/out" "$d/offline" KEK
  done
  check "with-ms: dbx.auth is byte-identical to efitools' (signed by KEK)" efitools_reproduces_auth dbx "$MS/out" "$MS/offline" KEK
fi

echo
echo "== 7. sign-efi: sbverify accepts the signed binary with db.crt =="
TEST_EFI="${VALIDATE_TEST_EFI:-}"
if [ -z "$TEST_EFI" ]; then
  for f in /usr/lib/efitools/*/HelloWorld.efi; do
    [ -f "$f" ] && TEST_EFI="$f" && break
  done
fi
if ! have_cmd sbverify || ! have_cmd sbattach; then
  skip_check "sbverify accepts the signed binary with db.crt" secureboot-tools "sbverify/sbattach not installed (sbsigntool)"
elif [ -z "$TEST_EFI" ] || [ ! -f "$TEST_EFI" ]; then
  skip_check "sbverify accepts the signed binary with db.crt" secureboot-tools \
    "no test EFI binary: efitools' HelloWorld.efi not found and VALIDATE_TEST_EFI unset"
else
  SIGNED="$WORK_DIR/signed.efi"
  check "the unsigned test binary fails sbverify (the check can fail)" fails sbverify --cert "$MS/out/db.crt" "$TEST_EFI"
  check "sign-efi exits 0" "$BOOTSTRAP_BIN" sign-efi --key "$MS/out/db.key" --cert "$MS/out/db.crt" --output "$SIGNED" "$TEST_EFI"
  check "sbverify --cert db.crt accepts it" sbverify --cert "$MS/out/db.crt" "$SIGNED"
  check "sbverify rejects it under the other hierarchy's db.crt" fails sbverify --cert "$NOMS/out/db.crt" "$SIGNED"
  check "sign-efi refuses an already-signed binary" \
    fails "$BOOTSTRAP_BIN" sign-efi --key "$MS/out/db.key" --cert "$MS/out/db.crt" --output "$WORK_DIR/twice.efi" "$SIGNED"
  check "sbattach detaches the signature" sbattach --detach "$WORK_DIR/sig.p7" "$SIGNED"
  openssl pkcs7 -inform DER -in "$WORK_DIR/sig.p7" -print_certs -out "$WORK_DIR/signer.pem" 2>/dev/null
  check "the signature's certificate is db.crt" same_cert "$WORK_DIR/signer.pem" <(openssl x509 -in "$MS/out/db.crt" -outform DER)
  check "the signer chains to PK.crt through KEK.crt" \
    openssl verify -CAfile "$MS/out/PK.crt" -untrusted "$MS/out/KEK.crt" "$WORK_DIR/signer.pem"
fi

summary
