#! /bin/sh
# Bridges $INCUS_BRIDGE_HOST_ADDR (a remote Incus TLS endpoint) to a unix
# socket at /socket/incus.sock.
#
# TLS verification stays ON. Incus serves a self-signed certificate, so the
# server's own certificate is pinned as the CA rather than verification being
# disabled, and the expected name is given explicitly because that certificate
# is issued for the cluster member's name (e.g. "pc0") and not for whatever
# address the client happens to dial.
set -eu

: "${INCUS_BRIDGE_HOST_ADDR:?required, e.g. 169.254.1.2:8443}"
SERVER_NAME="${INCUS_BRIDGE_SERVER_NAME:-pc0}"
SERVER_CERT="${INCUS_BRIDGE_SERVER_CERT:-/certs/servercerts/homelab-host.crt}"
CLIENT_CERT="${INCUS_BRIDGE_CLIENT_CERT:-/certs/client.crt}"
CLIENT_KEY="${INCUS_BRIDGE_CLIENT_KEY:-/certs/client.key}"
SOCKET="${INCUS_BRIDGE_SOCKET:-/socket/incus.sock}"

for f in "$SERVER_CERT" "$CLIENT_CERT" "$CLIENT_KEY"; do
	if [ ! -r "$f" ]; then
		echo "incus-bridge: cannot read $f — is the Incus config directory mounted at /certs?" >&2
		exit 1
	fi
done

# A socket left behind by an unclean stop would make socat fail to bind.
rm -f "$SOCKET"

echo "incus-bridge: $SOCKET -> $INCUS_BRIDGE_HOST_ADDR (verifying as '$SERVER_NAME' against $SERVER_CERT)"

# mode=600: the socket carries a trusted client certificate's authority, so it
# is readable only by the user this container runs as, which compose pins to
# the devcontainer's own uid.
exec socat \
	"UNIX-LISTEN:$SOCKET,fork,mode=600" \
	"OPENSSL:$INCUS_BRIDGE_HOST_ADDR,cert=$CLIENT_CERT,key=$CLIENT_KEY,cafile=$SERVER_CERT,openssl-commonname=$SERVER_NAME,verify=1"
