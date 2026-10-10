#! /bin/bash
# Validates GH issue #296: scripts/fetch-incusos-base.sh (`make incusos-base`)
# refuses an IncusOS base image it cannot verify, and leaves nothing behind
# that a later check would trust.
#
# The fetch's trust chain is: committed root-E1.crt -> update.sjson's S/MIME
# signature, by a certificate issued by the "Incus OS - Update E1" CA directly
# under that root -> the .img.gz's sha256 from the signed JSON -> the
# decompressed image's sidecar. Each link gets its own rejection case here, and
# each case requires BOTH a non-zero exit AND the error naming the link under
# test (the lesson of #115: "it exited non-zero" is satisfied by any failure at
# all, including a typo in the fixture). On top of that every rejection must
# print no `export` line and leave the cache directory empty.
#
# Hermetic by construction. It never downloads the real image, never reaches
# the network, and never reads or writes the real cache:
#   - every run of the fetch script goes through INCUSOS_CDN_URL=file://<tmp>
#     and INCUSOS_CACHE_DIR=<tmp>, and the version is one no CDN ever built;
#   - the signed manifests are made here, at run time, with a private PKI shaped
#     like upstream's (root -> CA -> yearly signing cert). Nothing under the
#     real Root E1 is signed by anything but the Update CA, so the "wrong
#     signer" cases cannot be built from real files;
#   - the committed root is still under test: one case checks that a perfectly
#     valid private chain is refused when INCUSOS_ROOT_CERT is NOT set, and one
#     pins the committed file's fingerprint.
#
# What this does not prove: that the real CDN's files verify against the
# committed root. `make incusos-base` proves that every time it fetches, and
# bump-incusos.yml's daily `--latest-stable` would fail loudly if it stopped.
#
# No compose stack, no Incus, no Go. Needs curl, openssl, jq, gzip, sha256sum.

set -uo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"
# shellcheck source=scripts/validate/lib.sh
. "$ROOT_DIR/scripts/validate/lib.sh"

VALIDATE_PROVES="make incusos-base rejects an unverifiable or corrupt IncusOS image and caches nothing (#296)"
VALIDATE_GROUP="none"
VALIDATE_NEEDS="curl openssl jq gzip sha256sum"
VALIDATE_DURATION="~5s"

validate_parse_args "$@"

# The fetch script's own tools. Without them nothing here can run at all, so
# this is a hard prerequisite (exit 2), like `require_cmd go` in the sibling
# group-none scripts, not a per-check skip.
require_cmd curl openssl jq gzip sha256sum
check_prereqs

FETCH="$ROOT_DIR/scripts/fetch-incusos-base.sh"
COMMITTED_ROOT="$ROOT_DIR/scripts/incusos-keys/root-E1.crt"
# Root E1's SHA-256 fingerprint, as recorded in #296 and in README.md.
ROOT_FINGERPRINT="10:D0:90:00:12:CF:8E:60:E0:23:B4:C2:96:32:57:63:59:63:9F:F9:F2:53:24:68:4C:74:4C:8A:0C:EC:8D:47"

# Versions no CDN build ever carried: even if a seam were ignored, there would
# be nothing real to download or to find in the real cache.
V=200001010000
OTHER_V=200001010001

WORK_DIR="$(mktemp -d)"
PKI="$WORK_DIR/pki"
cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# run_fetch <cdn dir> <cache dir> [VAR=value ...] [-- fetch args] — the only way
# this script runs the fetch script: always a file:// CDN, always a temp cache.
# Without an INCUSOS_ROOT_CERT assignment it trusts the committed root.
run_fetch() {
  local cdn="$1" cache="$2"
  shift 2
  local -a assign=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do
    assign+=("$1")
    shift
  done
  [ "${1:-}" != "--" ] || shift
  env -u INCUSOS_ROOT_CERT \
    INCUSOS_CDN_URL="file://$cdn" INCUSOS_CACHE_DIR="$cache" INCUSOS_VERSION="$V" \
    "${assign[@]}" "$FETCH" "$@"
}

# expect_reject <desc> <stderr pattern> <cdn dir> [VAR=value ...] — a fetch into
# a fresh cache that must fail for the stated reason and leave nothing behind.
REJECT_N=0
expect_reject() {
  local desc="$1" pattern="$2" cdn="$3"
  shift 3
  REJECT_N=$((REJECT_N + 1))
  local cache="$WORK_DIR/cache-reject-$REJECT_N" out rc
  out=$(run_fetch "$cdn" "$cache" "$@" 2>&1)
  rc=$?
  if [ "$rc" -eq 0 ]; then
    record_fail "$desc" "the fetch succeeded; expected it to refuse"
  elif ! grep -qF "$pattern" <<<"$out"; then
    record_fail "$desc" "refused, but not for the reason under test — want: $pattern; got: $(grep ERROR <<<"$out" | head -n 1)"
  elif grep -q '^export ' <<<"$out"; then
    record_fail "$desc" "refused, but still printed an export line"
  elif [ -n "$(find "$cache" -mindepth 1 2>/dev/null | head -n 1)" ]; then
    record_fail "$desc" "refused, but left something in the cache: $(find "$cache" -mindepth 1 | head -n 3 | tr '\n' ' ')"
  else
    record_pass "$desc"
  fi
}

# --- a private PKI shaped like upstream's -----------------------------------
mkkey() {
  openssl ecparam -name prime256v1 -genkey -noout -out "$PKI/$1.key" 2>/dev/null
}

# issue <name> <subject CN> <issuer name> <ca|leaf>
issue() {
  mkkey "$1"
  openssl req -new -key "$PKI/$1.key" -subj "/CN=$2/O=Linux Containers" -out "$PKI/$1.csr" 2>/dev/null
  openssl x509 -req -in "$PKI/$1.csr" -CA "$PKI/$3.crt" -CAkey "$PKI/$3.key" -CAcreateserial \
    -days 2 -extfile "$PKI/ext.cnf" -extensions "$4" -out "$PKI/$1.crt" 2>/dev/null
}

make_pki() {
  mkdir -p "$PKI"
  cat >"$PKI/ext.cnf" <<'EOF'
[ca]
basicConstraints=critical,CA:TRUE
keyUsage=critical,keyCertSign,cRLSign
[leaf]
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
EOF
  mkkey root
  openssl req -x509 -new -key "$PKI/root.key" -subj "/CN=Validate Root/O=Linux Containers" -days 2 \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -out "$PKI/root.crt" 2>/dev/null
  # The shape the fetch script accepts: root -> "Update E1" CA -> signing cert.
  issue updateca "Incus OS - Update E1" root ca
  issue updleaf "Incus OS - Update 2026 E1" updateca leaf
  # Under the same root, but not through the Update CA.
  issue otherca "Incus OS - SecureBoot E1" root ca
  issue otherleaf "Incus OS - Update 2026 E1" otherca leaf # right leaf name, wrong CA
  issue directleaf "Incus OS - Update E1" root leaf        # the CA's name, on a leaf the root issued itself
  issue nestedca "Incus OS - Update E1" otherca ca         # an "Update E1" CA another CA minted
  issue nestedleaf "Incus OS - Update 2026 E1" nestedca leaf
}

# --- a fake CDN ---------------------------------------------------------------
# make_image <cdn dir> <version> — a small stand-in image, gzipped where the
# CDN would have it. The plain bytes stay in $WORK_DIR/img.raw for comparison.
make_image() {
  mkdir -p "$1/$2/x86_64"
  head -c 65536 /dev/urandom >"$WORK_DIR/img.raw"
  gzip -c "$WORK_DIR/img.raw" >"$1/$2/x86_64/IncusOS_$2.img.gz"
}

# sign_cdn <cdn dir> <version> <signing cert> <chain cert>... — writes
# <version>/update.sjson naming whatever .img.gz is there (with its true
# sha256) and an index.sjson, both S/MIME-signed like the CDN's.
sign_cdn() {
  local cdn="$1" v="$2" leaf="$3" sum
  shift 3
  sum=$(sha256sum "$cdn/$v/x86_64/IncusOS_$v.img.gz" | cut -d' ' -f1)
  cat "$@" >"$WORK_DIR/chain.pem"
  printf '{"format":"1.0","channels":["stable"],"version":"%s","files":[{"filename":"x86_64/IncusOS_%s.img.gz","sha256":"%s"}]}' \
    "$v" "$v" "$sum" >"$WORK_DIR/update.json"
  openssl smime -sign -text -in "$WORK_DIR/update.json" -signer "$PKI/$leaf.crt" -inkey "$PKI/$leaf.key" \
    -certfile "$WORK_DIR/chain.pem" -out "$cdn/$v/update.sjson" 2>/dev/null
  # A newer `daily` build sits beside the stable ones: --latest-stable must not pick it.
  printf '{"format":"1.0","updates":[{"version":"199912310000","channels":["stable"]},{"version":"%s","channels":["testing","stable"]},{"version":"209901010000","channels":["daily"]}]}' \
    "$v" >"$WORK_DIR/index.json"
  openssl smime -sign -text -in "$WORK_DIR/index.json" -signer "$PKI/$leaf.crt" -inkey "$PKI/$leaf.key" \
    -certfile "$WORK_DIR/chain.pem" -out "$cdn/index.sjson" 2>/dev/null
}

# make_cdn <cdn dir> <signing cert> <chain cert>... — image + signatures for $V.
make_cdn() {
  local cdn="$1"
  shift
  make_image "$cdn" "$V"
  sign_cdn "$cdn" "$V" "$@"
}

echo "== 0. The committed trust root, and a private PKI to sign with =="
check_eq "committed root-E1.crt has the recorded SHA-256 fingerprint" "$ROOT_FINGERPRINT" \
  "$(openssl x509 -in "$COMMITTED_ROOT" -noout -fingerprint -sha256 2>&1 | sed 's/^[^=]*=//')"

make_pki
if openssl verify -CAfile "$PKI/root.crt" -no-CApath -no-CAstore \
  -untrusted "$PKI/updateca.crt" "$PKI/updleaf.crt" >/dev/null 2>&1; then
  record_pass "private PKI generated (root -> 'Incus OS - Update E1' CA -> signing cert)"
else
  # Every case below signs with it; their failures would only repeat this one.
  record_fail "private PKI generated (root -> 'Incus OS - Update E1' CA -> signing cert)" \
    "openssl could not build it; nothing below can run"
  summary
fi

GOOD="$WORK_DIR/cdn-good"
make_cdn "$GOOD" updleaf "$PKI/updateca.crt"

echo
echo "== 1. Positive controls: the same seams accept a correctly signed image =="
# Without these, every rejection below could be the fake CDN being broken.
CACHE_GOOD="$WORK_DIR/cache-good"
WANT_EXPORT="export INCUSOS_BASE_IMAGE=$CACHE_GOOD/$V/IncusOS_$V.img"
out=$(run_fetch "$GOOD" "$CACHE_GOOD" INCUSOS_ROOT_CERT="$PKI/root.crt" 2>"$WORK_DIR/good.err")
check_eq "fetch of a correctly signed image exits 0 and prints only the export line" \
  "rc=0 $WANT_EXPORT" "rc=$? $out"
check "the cached image is the decompressed download" cmp -s "$WORK_DIR/img.raw" "$CACHE_GOOD/$V/IncusOS_$V.img"
check "its sidecar matches it" bash -c "cd '$CACHE_GOOD/$V' && sha256sum --status -c 'IncusOS_$V.img.sha256'"
check_eq "all three test seams are announced on stderr" "3" "$(grep -c '^NOTE: INCUSOS_' "$WORK_DIR/good.err")"
check_eq "no temp directory is left beside the cache" "" "$(find "$CACHE_GOOD" -name '.fetch.*')"

out=$(run_fetch "$WORK_DIR/nowhere" "$CACHE_GOOD" INCUSOS_ROOT_CERT="$PKI/root.crt" 2>"$WORK_DIR/again.err")
check_eq "a second run is a no-op that needs no CDN at all" "rc=0 $WANT_EXPORT" "rc=$? $out"
check "and says it downloaded nothing" grep -q 'already cached; nothing to download' "$WORK_DIR/again.err"

check_eq "--latest-stable prints the newest stable version, not a newer daily" "$V" \
  "$(run_fetch "$GOOD" "$WORK_DIR/cache-unused" INCUSOS_ROOT_CERT="$PKI/root.crt" -- --latest-stable 2>/dev/null)"

echo
echo "== 2. update.sjson that does not verify =="
FLIP="$WORK_DIR/cdn-flip"
cp -r "$GOOD" "$FLIP"
sum=$(sha256sum "$GOOD/$V/x86_64/IncusOS_$V.img.gz" | cut -d' ' -f1)
[ "${sum:0:1}" = 0 ] && flipped="1${sum:1}" || flipped="0${sum:1}"
sed -i "s/$sum/$flipped/" "$FLIP/$V/update.sjson"
if cmp -s "$GOOD/$V/update.sjson" "$FLIP/$V/update.sjson"; then
  record_fail "one hex digit of the signed sha256 flipped" "the fixture was not changed by the flip"
else
  expect_reject "one hex digit of the signed sha256 flipped" \
    "signature verification failed" "$FLIP" INCUSOS_ROOT_CERT="$PKI/root.crt"
fi

PLAIN="$WORK_DIR/cdn-plain"
cp -r "$GOOD" "$PLAIN"
cp "$WORK_DIR/update.json" "$PLAIN/$V/update.sjson"
expect_reject "the manifest served unsigned, as plain JSON" \
  "signature verification failed" "$PLAIN" INCUSOS_ROOT_CERT="$PKI/root.crt"

# The default trust root is the committed one: a chain that is valid in every
# other way is refused unless INCUSOS_ROOT_CERT says otherwise.
expect_reject "a valid chain under a foreign root, checked against the committed root-E1.crt" \
  "signature verification failed" "$GOOD"

echo
echo "== 3. Validly signed under the trust root, but not through the Update CA =="
NOT_UPDATE_CA="not by a certificate issued by the Update CA"
make_cdn "$WORK_DIR/cdn-other" otherleaf "$PKI/otherca.crt"
expect_reject "signer issued by another CA under the root (even named like a real signing cert)" \
  "$NOT_UPDATE_CA" "$WORK_DIR/cdn-other" INCUSOS_ROOT_CERT="$PKI/root.crt"

make_cdn "$WORK_DIR/cdn-direct" directleaf "$PKI/root.crt"
expect_reject "signer named 'Incus OS - Update E1' but issued by the root itself" \
  "$NOT_UPDATE_CA" "$WORK_DIR/cdn-direct" INCUSOS_ROOT_CERT="$PKI/root.crt"

make_cdn "$WORK_DIR/cdn-nested" nestedleaf "$PKI/nestedca.crt" "$PKI/otherca.crt"
expect_reject "signer issued by an 'Update E1' CA that another CA minted, not the root" \
  "$NOT_UPDATE_CA" "$WORK_DIR/cdn-nested" INCUSOS_ROOT_CERT="$PKI/root.crt"

out=$(run_fetch "$WORK_DIR/cdn-other" "$WORK_DIR/cache-unused" INCUSOS_ROOT_CERT="$PKI/root.crt" -- --latest-stable 2>"$WORK_DIR/latest.err")
check_eq "--latest-stable refuses an index signed outside the Update CA, printing no version" \
  "rc=1 " "rc=$? $out"
check "and says why" grep -qF "$NOT_UPDATE_CA" "$WORK_DIR/latest.err"

echo
echo "== 4. A verified manifest, but the wrong image =="
MISMATCH="$WORK_DIR/cdn-mismatch"
cp -r "$GOOD" "$MISMATCH"
head -c 4096 /dev/urandom | gzip -c >"$MISMATCH/$V/x86_64/IncusOS_$V.img.gz"
expect_reject ".img.gz whose sha256 is not the signed one" \
  "sha256 mismatch" "$MISMATCH" INCUSOS_ROOT_CERT="$PKI/root.crt"

# The filename in the manifest carries the version, so a validly signed
# manifest for one version cannot vouch for another.
REPLAY="$WORK_DIR/cdn-replay"
cp -r "$GOOD" "$REPLAY"
mkdir -p "$REPLAY/$OTHER_V/x86_64"
cp "$GOOD/$V/update.sjson" "$REPLAY/$OTHER_V/update.sjson"
cp "$GOOD/$V/x86_64/IncusOS_$V.img.gz" "$REPLAY/$OTHER_V/x86_64/IncusOS_$OTHER_V.img.gz"
expect_reject "a validly signed manifest replayed for a different version" \
  "has no sha256 for" "$REPLAY" INCUSOS_ROOT_CERT="$PKI/root.crt" INCUSOS_VERSION="$OTHER_V"

# The sha256 matches, so only decompression can fail: nothing half-written may
# reach the final path.
NOTGZ="$WORK_DIR/cdn-notgz"
mkdir -p "$NOTGZ/$V/x86_64"
head -c 4096 /dev/urandom >"$NOTGZ/$V/x86_64/IncusOS_$V.img.gz"
sign_cdn "$NOTGZ" "$V" updleaf "$PKI/updateca.crt"
expect_reject "a correctly signed .img.gz that is not gzip data" \
  "could not decompress" "$NOTGZ" INCUSOS_ROOT_CERT="$PKI/root.crt"

expect_reject "a version that is not a 12-digit build stamp (it becomes a path)" \
  "is not an IncusOS version" "$GOOD" INCUSOS_ROOT_CERT="$PKI/root.crt" INCUSOS_VERSION="../$V"

echo
echo "== 5. A cache that no longer matches its sidecar is an error, never a re-fetch =="
# expect_stale_cache <desc> <cache dir> — the CDN is the GOOD one, so a silent
# re-fetch would succeed; the fetch must refuse and name the directory instead.
expect_stale_cache() {
  local desc="$1" cache="$2" out rc before after
  before=$(find "$cache" -type f -exec sha256sum {} + | sort)
  out=$(run_fetch "$GOOD" "$cache" INCUSOS_ROOT_CERT="$PKI/root.crt" 2>&1)
  rc=$?
  after=$(find "$cache" -type f -exec sha256sum {} + | sort)
  if [ "$rc" -eq 0 ]; then
    record_fail "$desc" "the fetch succeeded; expected it to refuse"
  elif ! grep -qF "rm -rf $(printf '%q' "$cache/$V")" <<<"$out"; then
    record_fail "$desc" "refused, but did not name the directory to delete: $(grep ERROR <<<"$out" | head -n 1)"
  elif grep -q '^export ' <<<"$out"; then
    record_fail "$desc" "refused, but still printed an export line"
  elif [ "$before" != "$after" ]; then
    record_fail "$desc" "refused, but changed the cache (a re-fetch, or a deletion)"
  else
    record_pass "$desc"
  fi
}

cp -r "$CACHE_GOOD" "$WORK_DIR/cache-tampered"
printf 'x' | dd of="$WORK_DIR/cache-tampered/$V/IncusOS_$V.img" bs=1 seek=100 conv=notrunc 2>/dev/null
if cmp -s "$CACHE_GOOD/$V/IncusOS_$V.img" "$WORK_DIR/cache-tampered/$V/IncusOS_$V.img"; then
  record_fail "cached image with one byte changed" "the fixture was not changed by the tamper"
else
  expect_stale_cache "cached image with one byte changed: names the directory, re-fetches nothing" \
    "$WORK_DIR/cache-tampered"
fi

cp -r "$CACHE_GOOD" "$WORK_DIR/cache-nosidecar"
rm "$WORK_DIR/cache-nosidecar/$V/IncusOS_$V.img.sha256"
expect_stale_cache "cached image whose sidecar is gone: names the directory, re-fetches nothing" \
  "$WORK_DIR/cache-nosidecar"

echo
echo "== 6. The real cache was never involved =="
# With the cache seam off, ask where these made-up versions would have landed.
for v in "$V" "$OTHER_V"; do
  real=$(env -u INCUSOS_CACHE_DIR INCUSOS_VERSION="$v" "$FETCH" --path 2>/dev/null)
  check "nothing for $v in the real cache ($(dirname "$(dirname "$real")"))" \
    bash -c "[ -n '$real' ] && [ ! -e '$(dirname "$real")' ]"
done

summary
