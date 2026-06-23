// Package ipam computes a Network's usable static-IPv4 pool — the
// dhcp_excluded_range carved out of DHCP for static use, minus the parent
// CIDR's network/broadcast addresses and the network's gateway if either
// falls inside it — and assigns/validates config.Instance.StaticIP against
// it. IPv4-only for v1.
//
// This is business logic, not persistence: callers (internal/server) read
// the prior store snapshot, call Assign to fill in/validate StaticIP on the
// in-memory config.Config, and only then persist via internal/store.
package ipam

import (
	"bytes"
	"fmt"
	"net"
	"strings"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// ExcludedRangeBounds parses netCfg.DHCPExcludedRange (format
// "<start>-<end>", e.g. "192.168.1.200-192.168.1.250") if set, and
// validates it lies within netCfg.CIDR. It returns nil bounds and no error
// if DHCPExcludedRange is unset — the range, and so the static pool, is
// optional.
func ExcludedRangeBounds(netCfg config.Network) (start, end net.IP, err error) {
	if netCfg.DHCPExcludedRange == "" {
		return nil, nil, nil
	}

	parts := strings.SplitN(netCfg.DHCPExcludedRange, "-", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("dhcp_excluded_range %q: want \"<start>-<end>\"", netCfg.DHCPExcludedRange)
	}
	start, end = net.ParseIP(strings.TrimSpace(parts[0])), net.ParseIP(strings.TrimSpace(parts[1]))
	if start == nil || end == nil {
		return nil, nil, fmt.Errorf("dhcp_excluded_range %q: not two valid IP addresses", netCfg.DHCPExcludedRange)
	}
	if bytes.Compare(start.To16(), end.To16()) > 0 {
		return nil, nil, fmt.Errorf("dhcp_excluded_range %q: start is after end", netCfg.DHCPExcludedRange)
	}

	_, ipNet, err := net.ParseCIDR(netCfg.CIDR)
	if err != nil {
		return nil, nil, fmt.Errorf("parse cidr %q: %w", netCfg.CIDR, err)
	}
	if !ipNet.Contains(start) || !ipNet.Contains(end) {
		return nil, nil, fmt.Errorf("dhcp_excluded_range %q is not contained in cidr %q", netCfg.DHCPExcludedRange, netCfg.CIDR)
	}
	return start, end, nil
}

// ValidateStaticIP validates that staticIP is a syntactically valid address
// contained in cidr — and, if excludedStart/excludedEnd are non-nil, within
// that dhcp_excluded_range too (static addresses are drawn from the
// excluded range, so DHCP never hands one out from under a node). It
// returns cidr's prefix length (e.g. 24 for a /24) for building a rendered
// address ("192.168.1.201/24").
func ValidateStaticIP(cidr, staticIP string, excludedStart, excludedEnd net.IP) (int, error) {
	ip := net.ParseIP(staticIP)
	if ip == nil {
		return 0, fmt.Errorf("static_ip %q is not a valid IP address", staticIP)
	}
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	if !ipNet.Contains(ip) {
		return 0, fmt.Errorf("static_ip %q is not contained in cidr %q: %w", staticIP, cidr, ErrOutOfRange)
	}
	if excludedStart != nil && (bytes.Compare(ip.To16(), excludedStart.To16()) < 0 || bytes.Compare(ip.To16(), excludedEnd.To16()) > 0) {
		return 0, fmt.Errorf("static_ip %q is outside dhcp_excluded_range %s-%s: %w", staticIP, excludedStart, excludedEnd, ErrOutOfRange)
	}
	size, _ := ipNet.Mask.Size()
	return size, nil
}

// NetworkPool is a parsed Network ready to validate static_ips against and
// (if DHCPExcludedRange is set) draw a usable static pool from.
type NetworkPool struct {
	netCfg                     config.Network
	ipNet                      *net.IPNet
	excludedStart, excludedEnd net.IP
	gateway                    net.IP
}

// NewNetworkPool parses n.CIDR and n.DHCPExcludedRange. IPv6 CIDRs return an
// error — IPAM is IPv4-only for v1.
func NewNetworkPool(n config.Network) (*NetworkPool, error) {
	_, ipNet, err := net.ParseCIDR(n.CIDR)
	if err != nil {
		return nil, fmt.Errorf("network %q: parse cidr %q: %w", n.Name, n.CIDR, err)
	}
	if ipNet.IP.To4() == nil {
		return nil, fmt.Errorf("network %q: ipv6 cidr %q not supported", n.Name, n.CIDR)
	}

	start, end, err := ExcludedRangeBounds(n)
	if err != nil {
		return nil, fmt.Errorf("network %q: %w", n.Name, err)
	}

	var gateway net.IP
	if n.Gateway != "" {
		gateway = net.ParseIP(n.Gateway)
	}

	return &NetworkPool{netCfg: n, ipNet: ipNet, excludedStart: start, excludedEnd: end, gateway: gateway}, nil
}

// UsableIPs returns the candidate static pool — DHCPExcludedRange minus the
// parent CIDR's network/broadcast addresses and the gateway, if any of
// those fall inside it — in deterministic ascending order. It returns nil
// if DHCPExcludedRange is unset: there is no auto-assignment pool for that
// network.
func (p *NetworkPool) UsableIPs() []net.IP {
	if p.excludedStart == nil {
		return nil
	}

	network := p.ipNet.IP.Mask(p.ipNet.Mask).To4()
	broadcast := broadcastAddr(p.ipNet)

	var out []net.IP
	for ip := p.excludedStart.To4(); compareIP(ip, p.excludedEnd) <= 0; ip = nextIP(ip) {
		if ip.Equal(network) || ip.Equal(broadcast) || (p.gateway != nil && ip.Equal(p.gateway)) {
			continue
		}
		dup := make(net.IP, len(ip))
		copy(dup, ip)
		out = append(out, dup)
	}
	return out
}

// validate checks ip against the network's CIDR and, if set,
// dhcp_excluded_range.
func (p *NetworkPool) validate(ip net.IP) error {
	_, err := ValidateStaticIP(p.netCfg.CIDR, ip.String(), p.excludedStart, p.excludedEnd)
	return err
}

// DetectDuplicates flags two instances on the same Network that both
// explicitly request the same static_ip. Instances on different networks
// are never compared against each other — two sites can legitimately reuse
// the same address.
func DetectDuplicates(instances []config.Instance) error {
	seen := make(map[string]map[string]string) // network -> static_ip -> instance name
	for _, inst := range instances {
		if inst.StaticIP == "" {
			continue
		}
		if seen[inst.Network] == nil {
			seen[inst.Network] = make(map[string]string)
		}
		if other, ok := seen[inst.Network][inst.StaticIP]; ok {
			return fmt.Errorf("instances %q and %q both request static_ip %s on network %q: %w",
				other, inst.Name, inst.StaticIP, inst.Network, ErrDuplicate)
		}
		seen[inst.Network][inst.StaticIP] = inst.Name
	}
	return nil
}

// Assign validates every explicit instances[i].StaticIP against its
// network and fills in the rest by drawing from that network's usable pool,
// mutating instances in place. prior is the previously persisted snapshot
// (e.g. from store.Instances), consulted so an instance that already has a
// valid assigned address keeps it across re-syncs rather than getting a
// different address each cycle — auto-assignment is reuse-then-fill, not
// pure next-free.
//
// Duplicate or out-of-range explicit static_ips, and pool exhaustion, are
// all hard failures: callers should not persist cfg if Assign returns an
// error.
func Assign(networks []config.Network, instances []config.Instance, prior []config.Instance) error {
	if err := DetectDuplicates(instances); err != nil {
		return err
	}

	netByName := make(map[string]config.Network, len(networks))
	for _, n := range networks {
		netByName[n.Name] = n
	}

	priorIPByName := make(map[string]net.IP, len(prior))
	for _, inst := range prior {
		if inst.StaticIP == "" {
			continue
		}
		if ip := net.ParseIP(inst.StaticIP); ip != nil {
			priorIPByName[inst.Name] = ip
		}
	}

	pools := make(map[string]*NetworkPool)
	usable := make(map[string][]net.IP)
	taken := make(map[string]map[string]net.IP) // network -> ip.String() -> ip

	getPool := func(network string) (*NetworkPool, error) {
		if p, ok := pools[network]; ok {
			return p, nil
		}
		n, ok := netByName[network]
		if !ok {
			return nil, fmt.Errorf("references unknown network %q", network)
		}
		p, err := NewNetworkPool(n)
		if err != nil {
			return nil, err
		}
		pools[network] = p
		usable[network] = p.UsableIPs()
		taken[network] = make(map[string]net.IP)
		return p, nil
	}

	// Pass 1: explicit static_ips. Validate and reserve them before any
	// auto-assignment runs, so auto-assigned instances never collide with
	// an operator's explicit choice.
	for i := range instances {
		inst := &instances[i]
		if inst.StaticIP == "" {
			continue
		}
		pool, err := getPool(inst.Network)
		if err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		ip := net.ParseIP(inst.StaticIP)
		if ip == nil {
			return fmt.Errorf("instance %q: static_ip %q is not a valid IP address: %w", inst.Name, inst.StaticIP, ErrOutOfRange)
		}
		if err := pool.validate(ip); err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		taken[inst.Network][ip.String()] = ip
	}

	// Pass 2: auto-assign the rest, reusing each instance's own prior
	// address when it's still valid, otherwise drawing the next free one.
	for i := range instances {
		inst := &instances[i]
		if inst.StaticIP != "" {
			continue
		}
		pool, err := getPool(inst.Network)
		if err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}

		if priorIP, ok := priorIPByName[inst.Name]; ok && pool.validate(priorIP) == nil {
			if _, conflict := taken[inst.Network][priorIP.String()]; !conflict {
				taken[inst.Network][priorIP.String()] = priorIP
				inst.StaticIP = priorIP.String()
				continue
			}
		}

		var picked net.IP
		for _, ip := range usable[inst.Network] {
			if _, used := taken[inst.Network][ip.String()]; !used {
				picked = ip
				break
			}
		}
		if picked == nil {
			return fmt.Errorf("instance %q: network %q: %w", inst.Name, inst.Network, ErrPoolExhausted)
		}
		taken[inst.Network][picked.String()] = picked
		inst.StaticIP = picked.String()
	}

	return nil
}

func compareIP(a, b net.IP) int {
	return bytes.Compare(a.To4(), b.To4())
}

func nextIP(ip net.IP) net.IP {
	v4 := ip.To4()
	out := make(net.IP, 4)
	copy(out, v4)
	for i := 3; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

func broadcastAddr(ipNet *net.IPNet) net.IP {
	ip := ipNet.IP.To4()
	mask := ipNet.Mask
	out := make(net.IP, 4)
	for i := range out {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}
