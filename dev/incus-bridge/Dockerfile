# Dev-only bridge: exposes a remote Incus daemon's TLS API as a local unix
# socket, so code that only speaks the unix socket (internal/incuslocal, and
# scripts/validate/incuslocal-round-trips-instances-over-unix-socket.sh) can be
# exercised against a real Incus without running on that host.
#
# Not part of the app's own image/binary — installing socat here is fine, and
# follows dev/git-fixture's precedent of building a tiny fixture image rather
# than pulling a third-party one (see #145 on pinning external images).
FROM alpine:3.20
RUN apk add --no-cache socat

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
