// Package ipam computes a Network's usable static-IPv4 pool — the
// dhcp_excluded_range carved out of DHCP for static use, minus the parent
// CIDR's network/broadcast addresses and the network's gateway if either
// falls inside it — and assigns/validates config.Instance.StaticIP against
// it. IPv4-only for v1.
//
// The config fields it reads are net/netip-typed and already
// syntactically valid (and, in the server sync path, semantically validated
// by config.Validate before Assign runs); this package adds the
// assignment-time concerns config.Validate can't express — duplicate
// detection, pool exhaustion, and stable reuse of a prior address across
// re-syncs.
//
// This is business logic, not persistence: callers (internal/server) read
// the prior store snapshot, call Assign to fill in/validate StaticIP on the
// in-memory config.Config, and only then persist via internal/store.
package ipam

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// NetworkPool is a Network ready to validate static_ips against and (if
// DHCPExcludedRange is set) draw a usable static pool from.
type NetworkPool struct {
	prefix                     netip.Prefix
	excludedStart, excludedEnd netip.Addr // zero when DHCPExcludedRange is unset
	gateway                    netip.Addr // zero when no gateway
}

// NewNetworkPool reads n.CIDR and n.DHCPExcludedRange. A missing/invalid or
// IPv6 CIDR returns an error — IPAM is IPv4-only for v1.
func NewNetworkPool(n config.Network) (*NetworkPool, error) {
	if !n.CIDR.IsValid() {
		return nil, fmt.Errorf("network %q: missing or invalid cidr", n.Name)
	}
	if !n.CIDR.Addr().Is4() {
		return nil, fmt.Errorf("network %q: ipv6 cidr %q not supported", n.Name, n.CIDR)
	}

	return &NetworkPool{
		prefix:        n.CIDR.Masked(),
		excludedStart: n.DHCPExcludedRange.Start,
		excludedEnd:   n.DHCPExcludedRange.End,
		gateway:       n.Gateway,
	}, nil
}

// UsableIPs returns the candidate static pool — DHCPExcludedRange minus the
// parent CIDR's network/broadcast addresses and the gateway, if any of those
// fall inside it — in deterministic ascending order. It returns nil if
// DHCPExcludedRange is unset: there is no auto-assignment pool for that
// network.
func (p *NetworkPool) UsableIPs() []netip.Addr {
	if !p.excludedStart.IsValid() {
		return nil
	}

	network := p.prefix.Addr()
	broadcast := lastAddr(p.prefix)

	var out []netip.Addr
	for ip := p.excludedStart; ip.Compare(p.excludedEnd) <= 0; ip = ip.Next() {
		if ip == network || ip == broadcast || ip == p.gateway {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// validate checks ip against the network's CIDR and, if set,
// dhcp_excluded_range. Static addresses are drawn from the excluded range, so
// DHCP never hands one out from under a node.
func (p *NetworkPool) validate(ip netip.Addr) error {
	if !p.prefix.Contains(ip) {
		return fmt.Errorf("static_ip %s is not contained in cidr %s: %w", ip, p.prefix, ErrOutOfRange)
	}
	if p.excludedStart.IsValid() && (ip.Compare(p.excludedStart) < 0 || ip.Compare(p.excludedEnd) > 0) {
		return fmt.Errorf("static_ip %s is outside dhcp_excluded_range %s-%s: %w", ip, p.excludedStart, p.excludedEnd, ErrOutOfRange)
	}
	return nil
}

// DetectDuplicates flags two instances on the same Network that both
// explicitly request the same static_ip. Instances on different networks are
// never compared against each other — two sites can legitimately reuse the
// same address.
func DetectDuplicates(instances []config.Instance) error {
	seen := make(map[string]map[netip.Addr]string) // network -> static_ip -> instance name
	for _, inst := range instances {
		if !inst.StaticIP.IsValid() {
			continue
		}
		if seen[inst.Network] == nil {
			seen[inst.Network] = make(map[netip.Addr]string)
		}
		if other, ok := seen[inst.Network][inst.StaticIP]; ok {
			return fmt.Errorf("instances %q and %q both request static_ip %s on network %q: %w",
				other, inst.Name, inst.StaticIP, inst.Network, ErrDuplicate)
		}
		seen[inst.Network][inst.StaticIP] = inst.Name
	}
	return nil
}

// Assign validates every explicit instances[i].StaticIP against its network
// and fills in the rest by drawing from that network's usable pool, mutating
// instances in place. prior is the previously persisted snapshot (e.g. from
// store.Instances), consulted so an instance that already has a valid assigned
// address keeps it across re-syncs rather than getting a different address
// each cycle — auto-assignment is reuse-then-fill, not pure next-free.
//
// Duplicate or out-of-range explicit static_ips, and pool exhaustion, are all
// hard failures: callers should not persist cfg if Assign returns an error.
func Assign(networks []config.Network, instances []config.Instance, prior []config.Instance) error {
	if err := DetectDuplicates(instances); err != nil {
		return err
	}

	netByName := make(map[string]config.Network, len(networks))
	for _, n := range networks {
		netByName[n.Name] = n
	}

	priorIPByName := make(map[string]netip.Addr, len(prior))
	priorOwner := make(map[string]map[netip.Addr]string) // network -> ip -> owning instance name
	for _, inst := range prior {
		if !inst.StaticIP.IsValid() {
			continue
		}
		priorIPByName[inst.Name] = inst.StaticIP
		if priorOwner[inst.Network] == nil {
			priorOwner[inst.Network] = make(map[netip.Addr]string)
		}
		priorOwner[inst.Network][inst.StaticIP] = inst.Name
	}

	pools := make(map[string]*NetworkPool)
	usable := make(map[string][]netip.Addr)
	taken := make(map[string]map[netip.Addr]bool) // network -> ip -> taken

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
		taken[network] = make(map[netip.Addr]bool)
		return p, nil
	}

	// Pass 1: explicit static_ips. Validate and reserve them before any
	// auto-assignment runs, so auto-assigned instances never collide with
	// an operator's explicit choice. A prior-assigned IP is treated as
	// reserved for its instance: a different instance's explicit static_ip
	// colliding with it is a hard failure, not a silent reassignment — see
	// docs/Ipam.md.
	for i := range instances {
		inst := &instances[i]
		if !inst.StaticIP.IsValid() {
			continue
		}
		pool, err := getPool(inst.Network)
		if err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		if err := pool.validate(inst.StaticIP); err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		if owner, ok := priorOwner[inst.Network][inst.StaticIP]; ok && owner != inst.Name {
			return fmt.Errorf("instance %q: static_ip %s is already assigned to instance %q on network %q: %w",
				inst.Name, inst.StaticIP, owner, inst.Network, ErrDuplicate)
		}
		taken[inst.Network][inst.StaticIP] = true
	}

	// Pass 2: auto-assign the rest, reusing each instance's own prior
	// address when it's still valid, otherwise drawing the next free one.
	for i := range instances {
		inst := &instances[i]
		if inst.StaticIP.IsValid() {
			continue
		}
		pool, err := getPool(inst.Network)
		if err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}

		if priorIP, ok := priorIPByName[inst.Name]; ok && pool.validate(priorIP) == nil {
			if !taken[inst.Network][priorIP] {
				taken[inst.Network][priorIP] = true
				inst.StaticIP = priorIP
				continue
			}
		}

		var picked netip.Addr
		for _, ip := range usable[inst.Network] {
			if !taken[inst.Network][ip] {
				picked = ip
				break
			}
		}
		if !picked.IsValid() {
			return fmt.Errorf("instance %q: network %q: %w", inst.Name, inst.Network, ErrPoolExhausted)
		}
		taken[inst.Network][picked] = true
		inst.StaticIP = picked
	}

	return nil
}

// lastAddr returns the broadcast (highest) address of an IPv4 prefix, e.g.
// 192.168.1.255 for 192.168.1.0/24. p must be a valid masked IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	v := binary.BigEndian.Uint32(a[:])
	v |= uint32(0xffffffff) >> p.Bits() // set every host bit
	binary.BigEndian.PutUint32(a[:], v)
	return netip.AddrFrom4(a)
}
