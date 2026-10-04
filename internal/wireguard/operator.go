package wireguard

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"strings"
)

// OperatorCIDR is the slice of OverlayCIDR reserved for operator peers
// (#195): the devices the operator reaches the web app's API from, over the
// tunnel. AssignTunnelIPs never hands an instance an address in it, so an
// operator's address can't be reassigned to a node, which would move the
// address to the node's key in the web app's peer table. Its top address is
// OverlayCIDR's broadcast address, so operators get .240 to .254: 15 devices.
var OperatorCIDR = netip.MustParsePrefix("10.100.0.240/28")

// OperatorPeer is one operator device that may call the web app's API over
// the tunnel: its WireGuard public key (from deployment config; the private
// half never reaches the web app, docs/Decisions.md §28) and the overlay
// address that key is allowed to send from.
type OperatorPeer struct {
	PublicKey PublicKey
	Addr      netip.Addr
}

// ParsePublicKey decodes a base64 WireGuard public key, the form `wg pubkey`
// prints.
func ParsePublicKey(b64 string) (PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return PublicKey{}, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != len(PublicKey{}) {
		return PublicKey{}, fmt.Errorf("public key is %d bytes, want %d", len(raw), len(PublicKey{}))
	}
	var pub PublicKey
	copy(pub[:], raw)
	return pub, nil
}

// ParseOperatorPeers parses the operator-peer list from deployment config:
// entries of the form <overlay-address>=<base64-public-key>, separated by
// commas and/or whitespace, e.g.
//
//	10.100.0.240=3lZ6...=,10.100.0.241=Xk2p...=
//
// Each address must lie in OperatorCIDR (and not be OverlayCIDR's broadcast
// address), and no address or key may appear twice: one key per address is
// what makes the source filter's address check identify a single key. An
// empty input is valid and yields no peers.
func ParseOperatorPeers(raw string) ([]OperatorPeer, error) {
	entries := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	broadcast := lastAddr(OverlayCIDR)

	peers := make([]OperatorPeer, 0, len(entries))
	seenAddr := make(map[netip.Addr]bool, len(entries))
	seenKey := make(map[PublicKey]bool, len(entries))
	for _, entry := range entries {
		addrStr, keyStr, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("operator peer %q: want <overlay-address>=<public-key>", entry)
		}
		addr, err := netip.ParseAddr(addrStr)
		if err != nil {
			return nil, fmt.Errorf("operator peer %q: address: %w", entry, err)
		}
		if !OperatorCIDR.Contains(addr) || addr == broadcast {
			return nil, fmt.Errorf("operator peer %q: address %s is outside the operator range %s (excluding %s)", entry, addr, OperatorCIDR, broadcast)
		}
		pub, err := ParsePublicKey(keyStr)
		if err != nil {
			return nil, fmt.Errorf("operator peer %q: %w", entry, err)
		}
		if seenAddr[addr] {
			return nil, fmt.Errorf("operator peer %q: address %s listed twice", entry, addr)
		}
		if seenKey[pub] {
			return nil, fmt.Errorf("operator peer %q: public key listed twice", entry)
		}
		seenAddr[addr], seenKey[pub] = true, true
		peers = append(peers, OperatorPeer{PublicKey: pub, Addr: addr})
	}
	return peers, nil
}
