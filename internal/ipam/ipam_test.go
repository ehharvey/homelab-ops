package ipam

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// rng is a small helper for building a config.Range from two address strings.
func rng(start, end string) config.Range {
	return config.Range{Start: netip.MustParseAddr(start), End: netip.MustParseAddr(end)}
}

func sampleNetwork() config.Network {
	return config.Network{
		Name:              "lan",
		CIDR:              netip.MustParsePrefix("192.168.1.0/24"),
		Gateway:           netip.MustParseAddr("192.168.1.1"),
		DHCPExcludedRange: rng("192.168.1.200", "192.168.1.203"),
	}
}

func TestNormalAssignment(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	instances := []config.Instance{{Name: "node-a", Network: "lan"}}

	if err := Assign(networks, instances, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got, want := instances[0].StaticIP.String(), "192.168.1.200"; got != want {
		t.Fatalf("StaticIP = %q, want %q", got, want)
	}
}

func TestPoolExhaustion(t *testing.T) {
	networks := []config.Network{sampleNetwork()} // 4-address pool: .200-.203
	instances := []config.Instance{
		{Name: "node-a", Network: "lan"},
		{Name: "node-b", Network: "lan"},
		{Name: "node-c", Network: "lan"},
		{Name: "node-d", Network: "lan"},
		{Name: "node-e", Network: "lan"},
	}

	err := Assign(networks, instances, nil)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Assign error = %v, want ErrPoolExhausted", err)
	}
}

func TestOutOfRangeSuppliedIP(t *testing.T) {
	cases := []struct {
		name     string
		staticIP string
	}{
		{"outside cidr", "10.0.0.5"},
		{"inside cidr, outside excluded range", "192.168.1.50"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			networks := []config.Network{sampleNetwork()}
			instances := []config.Instance{{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr(tc.staticIP)}}

			err := Assign(networks, instances, nil)
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("Assign error = %v, want ErrOutOfRange", err)
			}
		})
	}
}

func TestDuplicateSuppliedIPs(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	instances := []config.Instance{
		{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")},
		{Name: "node-b", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")},
	}

	if err := Assign(networks, instances, nil); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Assign error = %v, want ErrDuplicate", err)
	}
}

func TestDuplicateSuppliedIPsAcrossNetworksAllowed(t *testing.T) {
	networks := []config.Network{
		sampleNetwork(),
		{Name: "lan2", CIDR: netip.MustParsePrefix("192.168.2.0/24"), Gateway: netip.MustParseAddr("192.168.2.1"), DHCPExcludedRange: rng("192.168.2.200", "192.168.2.203")},
	}
	instances := []config.Instance{
		{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")},
		{Name: "node-b", Network: "lan2", StaticIP: netip.MustParseAddr("192.168.2.200")},
	}

	if err := Assign(networks, instances, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}
}

func TestAssignedAlreadyInStore(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	instances := []config.Instance{
		{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")},
		{Name: "node-b", Network: "lan"}, // auto-assign, must skip .200
	}

	if err := Assign(networks, instances, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got, want := instances[1].StaticIP.String(), "192.168.1.201"; got != want {
		t.Fatalf("node-b StaticIP = %q, want %q", got, want)
	}
}

func TestNoDHCPExcludedRange(t *testing.T) {
	net := sampleNetwork()
	net.DHCPExcludedRange = config.Range{}
	networks := []config.Network{net}

	t.Run("auto-assign unavailable", func(t *testing.T) {
		instances := []config.Instance{{Name: "node-a", Network: "lan"}}
		if err := Assign(networks, instances, nil); !errors.Is(err, ErrPoolExhausted) {
			t.Fatalf("Assign error = %v, want ErrPoolExhausted", err)
		}
	})

	t.Run("explicit static_ip still validated against cidr", func(t *testing.T) {
		instances := []config.Instance{{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.50")}}
		if err := Assign(networks, instances, nil); err != nil {
			t.Fatalf("Assign: %v", err)
		}

		instances = []config.Instance{{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("10.0.0.5")}}
		if err := Assign(networks, instances, nil); !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("Assign error = %v, want ErrOutOfRange", err)
		}
	})
}

func TestAssignmentStableAcrossResyncs(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	instances := []config.Instance{{Name: "node-a", Network: "lan"}}

	if err := Assign(networks, instances, nil); err != nil {
		t.Fatalf("first Assign: %v", err)
	}
	first := instances[0].StaticIP

	// Re-sync: fresh YAML parse, still no explicit static_ip, but the
	// prior store snapshot now reflects the previous run's assignment.
	prior := []config.Instance{{Name: "node-a", Network: "lan", StaticIP: first}}
	instances = []config.Instance{{Name: "node-a", Network: "lan"}}
	if err := Assign(networks, instances, prior); err != nil {
		t.Fatalf("second Assign: %v", err)
	}

	if instances[0].StaticIP != first {
		t.Fatalf("StaticIP churned across resync: first %q, second %q", first, instances[0].StaticIP)
	}
}

func TestAssignmentRedrawnWhenPriorIPNoLongerValid(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	prior := []config.Instance{{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.50")}} // now outside the excluded range
	instances := []config.Instance{{Name: "node-a", Network: "lan"}}

	if err := Assign(networks, instances, prior); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if instances[0].StaticIP.String() == "192.168.1.50" {
		t.Fatalf("stale out-of-pool prior IP was reused, want a fresh draw")
	}
	if got, want := instances[0].StaticIP.String(), "192.168.1.200"; got != want {
		t.Fatalf("StaticIP = %q, want %q", got, want)
	}
}

func TestExplicitStaticIPRejectedWhenConflictsWithPriorAssignedIP(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	prior := []config.Instance{
		{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")},
	}
	instances := []config.Instance{
		{Name: "node-a", Network: "lan"},
		{Name: "node-b", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")}, // reserved for node-a
	}

	// Expected behaviour: Assign should reject an explicit static_ip that
	// conflicts with a prior-assigned IP owned by a different instance,
	// rather than silently relocating the prior holder to a new address.
	if err := Assign(networks, instances, prior); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Assign error = %v, want ErrDuplicate", err)
	}
}

func TestExplicitStaticIPReassertingOwnPriorIPAccepted(t *testing.T) {
	networks := []config.Network{sampleNetwork()}
	prior := []config.Instance{{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")}}
	instances := []config.Instance{{Name: "node-a", Network: "lan", StaticIP: netip.MustParseAddr("192.168.1.200")}}

	// An instance explicitly reasserting its own prior-assigned IP is never
	// a conflict, even though that IP is "reserved" against other instances.
	if err := Assign(networks, instances, prior); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got, want := instances[0].StaticIP.String(), "192.168.1.200"; got != want {
		t.Fatalf("StaticIP = %q, want %q", got, want)
	}
}
