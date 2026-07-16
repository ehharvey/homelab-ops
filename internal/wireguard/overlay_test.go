package wireguard

import (
	"net/netip"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

func TestAssignTunnelIPsAssignsDistinctAddresses(t *testing.T) {
	instances := []config.Instance{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	if err := AssignTunnelIPs(instances, nil); err != nil {
		t.Fatalf("AssignTunnelIPs: %v", err)
	}

	seen := make(map[netip.Addr]string)
	for _, inst := range instances {
		if !inst.TunnelIP.IsValid() {
			t.Errorf("instance %q has no tunnel_ip assigned", inst.Name)
			continue
		}
		if !OverlayCIDR.Contains(inst.TunnelIP) {
			t.Errorf("instance %q tunnel_ip %s is outside %s", inst.Name, inst.TunnelIP, OverlayCIDR)
		}
		if inst.TunnelIP == WebAppAddr {
			t.Errorf("instance %q was assigned WebAppAddr %s", inst.Name, WebAppAddr)
		}
		if other, ok := seen[inst.TunnelIP]; ok {
			t.Errorf("instances %q and %q both got tunnel_ip %s", other, inst.Name, inst.TunnelIP)
		}
		seen[inst.TunnelIP] = inst.Name
	}
}

func TestAssignTunnelIPsReusesPriorAddressAcrossResync(t *testing.T) {
	instances := []config.Instance{{Name: "a"}, {Name: "b"}}
	if err := AssignTunnelIPs(instances, nil); err != nil {
		t.Fatalf("first AssignTunnelIPs: %v", err)
	}
	firstA, firstB := instances[0].TunnelIP, instances[1].TunnelIP

	prior := []config.Instance{
		{Name: "a", TunnelIP: firstA},
		{Name: "b", TunnelIP: firstB},
	}
	// Re-sync in reversed order plus a brand-new instance, to prove reuse
	// isn't just "first instance keeps first address" by coincidence.
	resynced := []config.Instance{{Name: "b"}, {Name: "a"}, {Name: "c"}}
	if err := AssignTunnelIPs(resynced, prior); err != nil {
		t.Fatalf("second AssignTunnelIPs: %v", err)
	}

	byName := map[string]netip.Addr{}
	for _, inst := range resynced {
		byName[inst.Name] = inst.TunnelIP
	}
	if byName["a"] != firstA {
		t.Errorf("instance a's tunnel_ip changed across resync: %s != %s", byName["a"], firstA)
	}
	if byName["b"] != firstB {
		t.Errorf("instance b's tunnel_ip changed across resync: %s != %s", byName["b"], firstB)
	}
	if byName["c"] == firstA || byName["c"] == firstB {
		t.Errorf("new instance c collided with a reused address: %s", byName["c"])
	}
}

func TestAssignTunnelIPsExhaustion(t *testing.T) {
	// Swap in a tiny overlay (2 usable addresses, one of which is
	// WebAppAddr) so exhaustion is reachable without generating hundreds of
	// instances. Package-level vars, so save/restore around the test.
	origCIDR, origAddr := OverlayCIDR, WebAppAddr
	t.Cleanup(func() { OverlayCIDR, WebAppAddr = origCIDR, origAddr })
	OverlayCIDR = netip.MustParsePrefix("10.100.0.0/30") // .0 network, .1-.2 usable, .3 broadcast
	WebAppAddr = netip.MustParseAddr("10.100.0.1")

	instances := []config.Instance{{Name: "a"}, {Name: "b"}}
	if err := AssignTunnelIPs(instances, nil); err == nil {
		t.Fatal("expected pool exhaustion error (only one usable address for two instances), got nil")
	}
}

func TestAssignTunnelIPsIgnoresPriorAddressOutsideCurrentOverlay(t *testing.T) {
	// If OverlayCIDR ever changes between deployments, a stale prior
	// assignment from the old range must not be reused verbatim.
	prior := []config.Instance{{Name: "a", TunnelIP: netip.MustParseAddr("192.0.2.1")}}
	instances := []config.Instance{{Name: "a"}}

	if err := AssignTunnelIPs(instances, prior); err != nil {
		t.Fatalf("AssignTunnelIPs: %v", err)
	}
	if !OverlayCIDR.Contains(instances[0].TunnelIP) {
		t.Errorf("tunnel_ip %s is outside %s despite an out-of-range prior value", instances[0].TunnelIP, OverlayCIDR)
	}
}
