package wireguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
)

// ClientOptions configures StartClient: a Tunnel that joins one remote peer
// as a client, rather than serving many as the web app does.
type ClientOptions struct {
	// PrivateKey is this client's own identity, e.g. from ReadPrivateKeyFile.
	PrivateKey PrivateKey
	// LocalAddr is this client's overlay address: the one the remote peer
	// lists for PrivateKey's public half. For an operator talking to the web
	// app, its WIREGUARD_OPERATOR_PEERS address, inside OperatorCIDR.
	LocalAddr netip.Addr
	// ListenPort is the local UDP port to bind; 0 lets the OS pick one.
	ListenPort int
	// PeerPublicKey, PeerTunnelIP and PeerEndpoint name the remote peer: its
	// public key, its overlay address (WebAppAddr for the web app) and the
	// host:port of its WireGuard UDP listener.
	PeerPublicKey PublicKey
	PeerTunnelIP  netip.Addr
	PeerEndpoint  netip.AddrPort
}

// StartClient starts a Tunnel and registers opts' remote peer with its
// endpoint, so this side can initiate the handshake: the remote peer's entry
// for this client carries no endpoint (an operator peer, or a node's seeded
// peer, never does). The handshake itself happens on the first packet, so the
// first dial may take a few seconds (internal/apiclient.RetryDial waits it
// out).
func StartClient(opts ClientOptions) (*Tunnel, error) {
	tun, err := Start(Options{PrivateKey: opts.PrivateKey, ListenPort: opts.ListenPort, LocalAddr: opts.LocalAddr})
	if err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}
	if err := tun.UpsertPeerWithEndpoint(opts.PeerPublicKey, opts.PeerTunnelIP, opts.PeerEndpoint); err != nil {
		tun.Close() //nolint:errcheck,gosec // best-effort cleanup on a failed setup; we're already returning the real error
		return nil, fmt.Errorf("register peer: %w", err)
	}
	return tun, nil
}

// ParsePrivateKey decodes a base64 WireGuard private key, the form
// `wg genkey` prints.
func ParsePrivateKey(b64 string) (PrivateKey, error) {
	pub, err := ParsePublicKey(b64)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("private key: %w", err)
	}
	return PrivateKey(pub), nil
}

// ReadPrivateKeyFile reads a base64 WireGuard private key from path. A key
// comes from a file, never a command-line flag, where `ps` would show it.
func ReadPrivateKeyFile(path string) (PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is operator-supplied (a --private-key-file flag), not untrusted input
	if err != nil {
		return PrivateKey{}, fmt.Errorf("read private key: %w", err)
	}
	priv, err := ParsePrivateKey(string(raw))
	if err != nil {
		return PrivateKey{}, fmt.Errorf("%s: %w", path, err)
	}
	return priv, nil
}

// ResolveEndpoints parses a peer's WireGuard endpoint, host:port, into every
// address it names. An IP literal is one address. A name is resolved here
// once, and every address it resolves to is returned in the resolver's order
// (an A and an AAAA record, say): which of them reaches the peer is learnt
// only by a handshake, so the caller tries them in turn.
func ResolveEndpoints(ctx context.Context, endpoint string) ([]netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(endpoint); err == nil {
		return []netip.AddrPort{ap}, nil
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	portNum, err := net.DefaultResolver.LookupPort(ctx, "udp", port)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: port: %w", endpoint, err)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: resolve host: %w", endpoint, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("endpoint %q: host has no addresses", endpoint)
	}
	eps := make([]netip.AddrPort, len(addrs))
	for i, a := range addrs {
		eps[i] = netip.AddrPortFrom(a.Unmap(), uint16(portNum)) //nolint:gosec // G115: LookupPort returns a port in 0-65535
	}
	return eps, nil
}
