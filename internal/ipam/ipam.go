// Package ipam computes the usable IPv4 pool a Network's nodes draw from and
// assigns/validates config.Instance.StaticIP against it. Which range that is
// depends on the network's dhcp.mode: the static range (dhcp_excluded_range)
// in off, proxy and reservations, the DHCP pool (dhcp.pool) in full, either
// written or derived as the other's complement, and carved around the
// network, broadcast, gateway and in-CIDR dns addresses. config's
// Network.Allocation computes all of that; this package reads it
// (docs/Ipam.md, #490). It also turns a network's assigned Instances into
// DHCP reservations (Reservations). IPv4-only in the 0.x line.
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
	"fmt"
	"net/netip"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// NetworkPool is a Network ready to validate static_ips against and draw
// node addresses from: the range config.Allocation.Nodes names for its
// dhcp.mode.
type NetworkPool struct {
	prefix netip.Prefix
	alloc  config.Allocation
	nodes  config.Span
}

// NewNetworkPool reads n's Allocation. A missing/invalid or IPv6 CIDR, or a
// range Allocation rejects, returns an error — IPAM is IPv4-only in the 0.x
// line.
func NewNetworkPool(n config.Network) (*NetworkPool, error) {
	if !n.CIDR.IsValid() {
		return nil, fmt.Errorf("network %q: missing or invalid cidr", n.Name)
	}
	if !n.CIDR.Addr().Is4() {
		return nil, fmt.Errorf("network %q: ipv6 cidr %q not supported", n.Name, n.CIDR)
	}
	alloc, issues := n.Allocation()
	if !issues.Empty() {
		return nil, fmt.Errorf("network %q: %w", n.Name, issues)
	}
	return &NetworkPool{prefix: n.CIDR.Masked(), alloc: alloc, nodes: alloc.Nodes()}, nil
}

// nodeCursor walks a NetworkPool's node range — its effective spans, already
// carved around the network, broadcast, gateway and in-CIDR dns addresses —
// in ascending order, one address per next, resuming where it left off. An
// unset node range (neither field written outside full mode) has no spans:
// no auto-assignment pool. Assign keeps one per network
// and draws each auto-assigned address from it, so a sync touches only the
// addresses it skips or hands out, not the whole range: in full mode with no
// range written that is the whole cidr, 16M addresses for a /8.
type nodeCursor struct {
	spans   []config.Range
	span    int        // index into spans of the span being walked
	ip      netip.Addr // the next address in spans[span], once started
	started bool       // ip holds a position in spans[span]
}

func (p *NetworkPool) cursor() *nodeCursor { return &nodeCursor{spans: p.nodes.Effective} }

// next returns the next address and true, or false once the range is spent.
func (c *nodeCursor) next() (netip.Addr, bool) {
	for c.span < len(c.spans) {
		r := c.spans[c.span]
		if !c.started {
			c.ip, c.started = r.Start, true
		}
		// ip.IsValid() guards the terminal case: Next() past the last
		// address (255.255.255.255) returns the zero Addr, which sorts
		// *before* r.End, so a range ending at the all-ones address would
		// otherwise loop forever.
		if c.ip.IsValid() && c.ip.Compare(r.End) <= 0 {
			ip := c.ip
			c.ip = ip.Next()
			return ip, true
		}
		c.span, c.started = c.span+1, false
	}
	return netip.Addr{}, false
}

// validate checks ip against the network's CIDR and, if set, the uncarved
// node range: the static range, which DHCP never hands out from under a
// node, or in full mode the pool, where each node's address is its
// reservation.
func (p *NetworkPool) validate(ip netip.Addr) error {
	if !p.prefix.Contains(ip) {
		return fmt.Errorf("static_ip %s is not contained in cidr %s: %w", ip, p.prefix, ErrOutOfRange)
	}
	if p.nodes.Source != config.SpanUnset && !p.nodes.ContainsUncarved(ip) {
		return fmt.Errorf("static_ip %s is outside %s: %w", ip, p.nodes.Describe(), ErrOutOfRange)
	}
	return nil
}

// reusable reports whether ip, an instance's prior address, may be handed
// back to it when its static_ip is omitted: valid for the network, and not
// an address that is never allocated (a prior explicit pin on an in-CIDR dns
// address, or a gateway that has since moved onto it).
func (p *NetworkPool) reusable(ip netip.Addr) bool {
	return p.validate(ip) == nil && !p.alloc.IsCarved(ip)
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
	// Each network's next-free cursor. Every address it has passed is taken,
	// and nothing is ever released, so the next free address is never
	// behind it.
	cursors := make(map[string]*nodeCursor)
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
		cursors[network] = p.cursor()
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

	// Pass 2: give each auto-assigned instance back its own prior address
	// while it's still valid. All of them, before any draw: a new instance
	// earlier in the order must not draw an address an existing node still
	// holds and renumber it (docs/Ipam.md).
	for i := range instances {
		inst := &instances[i]
		if inst.StaticIP.IsValid() {
			continue
		}
		pool, err := getPool(inst.Network)
		if err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		if priorIP, ok := priorIPByName[inst.Name]; ok && pool.reusable(priorIP) && !taken[inst.Network][priorIP] {
			taken[inst.Network][priorIP] = true
			inst.StaticIP = priorIP
		}
	}

	// Pass 3: draw the next free address for the rest.
	for i := range instances {
		inst := &instances[i]
		if inst.StaticIP.IsValid() {
			continue
		}
		var picked netip.Addr
		c := cursors[inst.Network]
		for ip, ok := c.next(); ok; ip, ok = c.next() {
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

// Reservation is one Instance's DHCP reservation: what Phase 5's Kea
// renderer serves, and what DNS records are made from (docs/Decisions.md
// §31).
type Reservation struct {
	// MAC is the Instance's mac, normalised (net.HardwareAddr.String:
	// lowercase, colon-separated).
	MAC string
	// IP is the Instance's assigned address.
	IP netip.Addr
	// Hostname is the bare Instance name, the validated lowercase label
	// that is already the node's hostname (#193).
	Hostname string
}

// Reservations returns the reservations of network's Instances among
// instances, in their order. instances must be post-Assign, as the store
// holds them (§12): an Instance on network with a mac but no address is an
// error (ErrUnassigned). One without a mac is skipped; config.Validate
// allows that only on off and proxy networks. It doesn't read the network's
// dhcp.mode: whether reservations are served is the consumer's call.
func Reservations(network string, instances []config.Instance) ([]Reservation, error) {
	var out []Reservation
	for _, inst := range instances {
		if inst.Network != network || inst.MAC == "" {
			continue
		}
		hw, err := config.ParseMAC(inst.MAC)
		if err != nil {
			return nil, fmt.Errorf("instance %q: mac %q: %w", inst.Name, inst.MAC, err)
		}
		if !inst.StaticIP.IsValid() {
			return nil, fmt.Errorf("instance %q on network %q: %w", inst.Name, network, ErrUnassigned)
		}
		out = append(out, Reservation{MAC: hw.String(), IP: inst.StaticIP, Hostname: inst.Name})
	}
	return out, nil
}
