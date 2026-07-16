package wireguard

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// AssignTunnelIPs fills in each instance's TunnelIP from OverlayCIDR,
// mutating instances in place. Unlike LAN static_ips (internal/ipam),
// tunnel IPs are never operator-supplied — config.Instance.TunnelIP is
// yaml:"-" — so there is no explicit-value pass: every instance is
// auto-assigned, reusing its own prior address (from prior, the
// last-synced snapshot) across re-syncs so it stays stable, or drawing the
// next free address from OverlayCIDR otherwise. Mirrors ipam.Assign's
// reuse-then-fill semantics, but against one fixed, app-wide pool rather
// than a pool per config.Network.
func AssignTunnelIPs(instances []config.Instance, prior []config.Instance) error {
	priorByName := make(map[string]netip.Addr, len(prior))
	for _, inst := range prior {
		if inst.TunnelIP.IsValid() {
			priorByName[inst.Name] = inst.TunnelIP
		}
	}

	network := OverlayCIDR.Masked().Addr()
	broadcast := lastAddr(OverlayCIDR)

	taken := make(map[netip.Addr]bool, len(instances))
	// Reserve every still-valid prior assignment up front, so reuse always
	// wins over fresh assignment regardless of instance order.
	for _, inst := range instances {
		if ip, ok := priorByName[inst.Name]; ok && inRange(ip, network, broadcast) {
			taken[ip] = true
		}
	}

	for i := range instances {
		inst := &instances[i]
		if ip, ok := priorByName[inst.Name]; ok && inRange(ip, network, broadcast) {
			inst.TunnelIP = ip
			continue
		}

		picked, err := nextFree(taken, network, broadcast)
		if err != nil {
			return fmt.Errorf("instance %q: %w", inst.Name, err)
		}
		taken[picked] = true
		inst.TunnelIP = picked
	}

	return nil
}

func inRange(ip, network, broadcast netip.Addr) bool {
	return OverlayCIDR.Contains(ip) && ip != network && ip != broadcast && ip != WebAppAddr
}

func nextFree(taken map[netip.Addr]bool, network, broadcast netip.Addr) (netip.Addr, error) {
	for ip := network.Next(); ip.IsValid() && ip.Compare(broadcast) < 0; ip = ip.Next() {
		if ip == WebAppAddr || taken[ip] {
			continue
		}
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("overlay address pool %s exhausted", OverlayCIDR)
}

// lastAddr reports p's broadcast address (all host bits set). IPv4-only,
// matching internal/ipam's identical helper and this repo's IPv4-only 0.x
// scope — duplicated rather than exported from internal/ipam, since the two
// packages' pool shapes (per-Network vs. one fixed app-wide pool) are
// different enough that sharing the type wouldn't simplify either side.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	v := binary.BigEndian.Uint32(a[:])
	v |= uint32(0xffffffff) >> p.Bits()
	binary.BigEndian.PutUint32(a[:], v)
	return netip.AddrFrom4(a)
}
