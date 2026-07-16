FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/web ./cmd/web

# flasher-tool is the upstream IncusOS image flasher the web app shells out to
# (internal/flasher). It is NOT a Go dependency of this module — it pulls in
# incus/v7's full dependency tree (see internal/flasher's package doc) — so it
# is built in its own stage and copied in as a standalone binary. Pinned to the
# same incus-os commit vendored via the third_party/incus-os submodule
# (10705332c6cf4eadf63be1b8db99d19f64bc0ca6), so the seed-tar layout it writes
# matches the incus-osd/api structs internal/seed renders against. Built
# CGO-free so it stays a static binary and the final image can remain
# distroless/static (verified: `ldd` reports "not a dynamic executable").
FROM golang:1.26 AS flasher
RUN CGO_ENABLED=0 go install \
    github.com/lxc/incus-os/incus-osd/cmd/flasher-tool@v0.0.0-20260623005315-10705332c6cf

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/web /web
COPY --from=flasher /go/bin/flasher-tool /flasher-tool
# Absolute path so the web app resolves flasher-tool without a $PATH (distroless
# has no shell/PATH). newImageBuilder reads this; unset in dev falls back to
# resolving "flasher-tool" from $PATH.
ENV FLASHER_TOOL_PATH=/flasher-tool
EXPOSE 8080
# WireGuard's IANA-assigned UDP port. internal/wireguard terminates the
# tunnel entirely in-process via a userspace network stack (no host TUN
# device) — no NET_ADMIN or other added capability needed, just this one
# plain UDP port.
EXPOSE 51820/udp
ENTRYPOINT ["/web"]
