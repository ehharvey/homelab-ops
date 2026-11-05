package wireguard

import (
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
// than a pool per config.Network. Addresses in OperatorCIDR are never
// assigned, and a prior assignment inside it is not reused (#195).
func AssignTunnelIPs(instances []config.Instance, prior []config.Instance) error {
	priorByName := make(map[string]netip.Addr, len(prior))
	for _, inst := range prior {
		if inst.TunnelIP.IsValid() {
			priorByName[inst.Name] = inst.TunnelIP
		}
	}

	network := OverlayCIDR.Masked().Addr()
	broadcast := config.LastAddr(OverlayCIDR)

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
	return OverlayCIDR.Contains(ip) && ip != network && ip != broadcast && !reserved(ip)
}

// reserved reports whether ip belongs to the web app (WebAppAddr) or to the
// operator peers (OperatorCIDR) rather than to the instance pool.
func reserved(ip netip.Addr) bool {
	return ip == WebAppAddr || OperatorCIDR.Contains(ip)
}

func nextFree(taken map[netip.Addr]bool, network, broadcast netip.Addr) (netip.Addr, error) {
	for ip := network.Next(); ip.IsValid() && ip.Compare(broadcast) < 0; ip = ip.Next() {
		if reserved(ip) || taken[ip] {
			continue
		}
		return ip, nil
	}
	return netip.Addr{}, fmt.Errorf("overlay address pool %s exhausted", OverlayCIDR)
}
