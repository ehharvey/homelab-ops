// Package wireguard runs an in-process, userspace WireGuard endpoint for the
// web app: it terminates the WireGuard protocol itself
// (golang.zx2c4.com/wireguard, the reference implementation Tailscale and
// wireguard-go build on) over a gVisor-backed virtual network stack
// (tun/netstack) instead of a host TUN device. That means no NET_ADMIN and
// no kernel wg-quick interface — the tunnel lives entirely inside this
// process, which is what keeps the deployment distroless (see
// docs/Decisions.md's WireGuard follow-up). Implementing the Noise protocol
// handshake/replay/rekeying ourselves would be a security-critical-crypto
// anti-pattern, so importing the upstream reference implementation directly
// is a deliberate exception to this repo's usual "prefer stdlib" rule.
package wireguard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// OverlayCIDR is the fixed, app-wide WireGuard tunnel address space every
// deployment uses. It is not operator config: it's a private overlay never
// routed on any LAN, so there is nothing for an operator to collide with.
var OverlayCIDR = netip.MustParsePrefix("10.100.0.0/24")

// WebAppAddr is the web app's own fixed address within OverlayCIDR.
var WebAppAddr = netip.MustParseAddr("10.100.0.1")

// mtu is conservative enough to survive being further encapsulated by
// whatever real-world transport carries the WireGuard UDP packets.
const mtu = 1420

// PrivateKey is a raw Curve25519 private key. Deliberately has no String()/
// fmt.Stringer implementation (unlike PublicKey) — a Stringer here would
// make accidental key leakage into a log or error message (via %v/%s on a
// containing struct) one careless format verb away. Callers that genuinely
// need the encoded form call Base64 explicitly.
type PrivateKey [32]byte

// Base64 renders k the same way `wg genkey`/wgctrl do, for embedding into
// IncusOS's network.yaml seed.
func (k PrivateKey) Base64() string { return base64.StdEncoding.EncodeToString(k[:]) }

// PublicKey is a raw Curve25519 public key. Not secret, so a Stringer is
// safe and convenient for logging/seed rendering.
type PublicKey [32]byte

// Base64 renders k the same way `wg pubkey`/wgctrl do.
func (k PublicKey) Base64() string { return base64.StdEncoding.EncodeToString(k[:]) }

// String makes PublicKey satisfy fmt.Stringer, so it prints as the familiar
// base64 form rather than a raw byte dump.
func (k PublicKey) String() string { return k.Base64() }

// hexKey renders k in the lowercase-hex form the device package's UAPI
// configuration protocol (IpcSet) expects — a different encoding than
// Base64, which is for the IncusOS seed/operator-facing form. Mixing these
// up produces a key wireguard-go or IncusOS silently fails to parse
// correctly, not a loud error, so keep this used only for IpcSet calls.
func hexKey(k [32]byte) string { return hex.EncodeToString(k[:]) }

// PublicKeyOf derives the Curve25519 public key for priv.
func PublicKeyOf(priv PrivateKey) PublicKey {
	var pub PublicKey
	curve25519.ScalarBaseMult((*[32]byte)(&pub), (*[32]byte)(&priv))
	return pub
}

// GenerateKeypair creates a fresh Curve25519 keypair, clamped per RFC7748
// the same way `wg genkey` does, entirely offline.
func GenerateKeypair() (PrivateKey, PublicKey, error) {
	var priv PrivateKey
	if _, err := rand.Read(priv[:]); err != nil {
		return PrivateKey{}, PublicKey{}, fmt.Errorf("generate private key: %w", err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	return priv, PublicKeyOf(priv), nil
}

// Options configures Start.
type Options struct {
	// PrivateKey is this endpoint's own identity.
	PrivateKey PrivateKey
	// ListenPort is the UDP port the tunnel binds. A plain unprivileged
	// net.ListenUDP under the hood (conn.NewDefaultBind) — no elevated
	// capability needed for a port >= 1024.
	ListenPort int
	// LocalAddr is this endpoint's address on the virtual network stack.
	// Every production caller passes WebAppAddr (only the web app itself
	// ever calls Start — nodes get their WireGuard config baked into
	// IncusOS's own network stack via network.yaml, never by running this
	// package). Configurable rather than hardcoded so two Tunnels can be
	// exercised against each other directly in tests.
	LocalAddr netip.Addr
}

// Tunnel is an in-process WireGuard endpoint: a userspace network stack
// (no host TUN device) with the wireguard-go protocol engine terminating
// traffic on it. The zero value is not usable; construct via Start.
type Tunnel struct {
	dev       *device.Device
	net       *netstack.Net
	publicKey PublicKey
}

// Start brings up a Tunnel bound to opts.ListenPort with local address
// opts.LocalAddr, using opts.PrivateKey as this endpoint's identity. The
// returned Tunnel owns background goroutines; call Close when done.
func Start(opts Options) (*Tunnel, error) {
	tunDev, nsNet, err := netstack.CreateNetTUN([]netip.Addr{opts.LocalAddr}, nil, mtu)
	if err != nil {
		return nil, fmt.Errorf("create virtual network stack: %w", err)
	}

	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wireguard: "))

	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\n", hexKey(opts.PrivateKey), opts.ListenPort)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure device: %w", err)
	}

	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("bring device up: %w", err)
	}

	return &Tunnel{dev: dev, net: nsNet, publicKey: PublicKeyOf(opts.PrivateKey)}, nil
}

// PublicKey reports this Tunnel's own public key — embedded into every
// node's seed as the peer the node should trust.
func (t *Tunnel) PublicKey() PublicKey { return t.publicKey }

// UpsertPeer registers pub as a trusted peer allowed to use tunnelIP.
// Idempotent: safe to call repeatedly for the same peer (e.g. on every
// config sync) without accumulating duplicate allowed-ip entries —
// replace_allowed_ips=true resets that peer's allowed-ip set before adding
// the current one, and the device package creates the peer if it doesn't
// already exist.
func (t *Tunnel) UpsertPeer(pub PublicKey, tunnelIP netip.Addr) error {
	uapi := fmt.Sprintf(
		"public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\n",
		hexKey(pub), tunnelIP,
	)
	if err := t.dev.IpcSet(uapi); err != nil {
		return fmt.Errorf("upsert peer %s: %w", pub, err)
	}
	return nil
}

// ListenPort reports the UDP port this Tunnel actually bound to — useful
// after Start(Options{ListenPort: 0}), where the OS picks the port.
func (t *Tunnel) ListenPort() (int, error) {
	conf, err := t.dev.IpcGet()
	if err != nil {
		return 0, fmt.Errorf("query device config: %w", err)
	}
	const prefix = "listen_port="
	for _, line := range strings.Split(conf, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			port, err := strconv.Atoi(rest)
			if err != nil {
				return 0, fmt.Errorf("parse %q: %w", line, err)
			}
			return port, nil
		}
	}
	return 0, fmt.Errorf("device config has no %s line", prefix)
}

// DialContext dials a TCP address (host:port, e.g. a node's tunnel IP and
// Incus's HTTPS port) through the tunnel's virtual network stack. This is
// the only way to reach a peer's tunnel address: the stack is entirely
// in-process with no host-level route to it, so nothing outside this
// process (not even another process in the same container) can dial
// through it — only Tunnel's own methods can.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("dial %s %s: only tcp is supported", network, address)
	}
	addrPort, err := resolveAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	conn, err := t.net.DialContextTCPAddrPort(ctx, addrPort)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	return conn, nil
}

func resolveAddrPort(address string) (netip.AddrPort, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return netip.AddrPort{}, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse host %q: %w", host, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse port %q: %w", portStr, err)
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// Close tears down the tunnel's device and virtual network stack.
func (t *Tunnel) Close() error {
	t.dev.Close()
	return nil
}
