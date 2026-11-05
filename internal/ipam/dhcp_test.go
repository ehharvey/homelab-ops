package ipam

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// dnsNetwork is the worked example of #490: 10.0.0.0/24, gateway .1, an
// in-cidr dns server at .53 and an outside one.
func dnsNetwork(mode config.DHCPMode, static, pool config.Range) config.Network {
	return config.Network{
		Name:              "lan",
		CIDR:              netip.MustParsePrefix("10.0.0.0/24"),
		Gateway:           netip.MustParseAddr("10.0.0.1"),
		DNS:               []netip.Addr{netip.MustParseAddr("10.0.0.53"), netip.MustParseAddr("1.1.1.1")},
		DHCPExcludedRange: static,
		DHCP:              config.DHCP{Mode: mode, Pool: pool},
	}
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// TestAutoAssignSkipsDNSAddress: an in-cidr dns address is carved out of
// the range auto-assignment draws from, in every mode.
func TestAutoAssignSkipsDNSAddress(t *testing.T) {
	for _, mode := range []config.DHCPMode{config.DHCPOff, config.DHCPProxy, config.DHCPReservations} {
		t.Run(string(mode), func(t *testing.T) {
			networks := []config.Network{dnsNetwork(mode, rng("10.0.0.52", "10.0.0.55"), config.Range{})}
			instances := []config.Instance{{Name: "a", Network: "lan"}, {Name: "b", Network: "lan"}, {Name: "c", Network: "lan"}}
			if err := Assign(networks, instances, nil); err != nil {
				t.Fatalf("Assign: %v", err)
			}
			got := []netip.Addr{instances[0].StaticIP, instances[1].StaticIP, instances[2].StaticIP}
			if want := addrs("10.0.0.52", "10.0.0.54", "10.0.0.55"); !reflect.DeepEqual(got, want) {
				t.Errorf("assigned %v, want %v (skipping dns 10.0.0.53)", got, want)
			}
		})
	}
}

// TestFullDrawsFromPool: in full, nodes draw from the pool, and with neither
// field written the pool is the whole cidr, carved.
func TestFullDrawsFromPool(t *testing.T) {
	t.Run("neither written", func(t *testing.T) {
		p, err := NewNetworkPool(dnsNetwork(config.DHCPFull, config.Range{}, config.Range{}))
		if err != nil {
			t.Fatalf("NewNetworkPool: %v", err)
		}
		usable := usableIPs(p)
		if len(usable) != 252 || usable[0] != netip.MustParseAddr("10.0.0.2") || usable[251] != netip.MustParseAddr("10.0.0.254") {
			t.Fatalf("usableIPs = %d addresses from %v to %v, want 252 from 10.0.0.2 to 10.0.0.254", len(usable), usable[0], usable[len(usable)-1])
		}
		for _, ip := range usable {
			if ip == netip.MustParseAddr("10.0.0.53") {
				t.Errorf("usableIPs holds the dns address %v", ip)
			}
		}
	})
	t.Run("static written, the pool derived", func(t *testing.T) {
		networks := []config.Network{dnsNetwork(config.DHCPFull, rng("10.0.0.0", "10.0.0.99"), config.Range{})}
		instances := []config.Instance{{Name: "a", Network: "lan"}}
		if err := Assign(networks, instances, nil); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		if got, want := instances[0].StaticIP, netip.MustParseAddr("10.0.0.100"); got != want {
			t.Errorf("assigned %v, want the derived pool's first address %v", got, want)
		}
	})
	t.Run("explicit static_ip outside the pool", func(t *testing.T) {
		networks := []config.Network{dnsNetwork(config.DHCPFull, rng("10.0.0.2", "10.0.0.99"), rng("10.0.0.100", "10.0.0.200"))}
		instances := []config.Instance{{Name: "a", Network: "lan", StaticIP: netip.MustParseAddr("10.0.0.20")}}
		if err := Assign(networks, instances, nil); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Assign = %v, want ErrOutOfRange", err)
		}
	})
	t.Run("pool exhausted", func(t *testing.T) {
		networks := []config.Network{dnsNetwork(config.DHCPFull, config.Range{}, rng("10.0.0.52", "10.0.0.53"))}
		instances := []config.Instance{{Name: "a", Network: "lan"}, {Name: "b", Network: "lan"}}
		if err := Assign(networks, instances, nil); !errors.Is(err, ErrPoolExhausted) {
			t.Errorf("Assign = %v, want ErrPoolExhausted (.53 is carved)", err)
		}
	})
}

// TestExplicitStaticIPOnDNSAddressAccepted: a node that is the DNS server
// may pin its own address, since explicit static_ips are checked against
// the uncarved range. Auto-assignment still skips it, and a prior pin on it
// isn't handed back once the static_ip is dropped.
func TestExplicitStaticIPOnDNSAddressAccepted(t *testing.T) {
	for _, n := range []config.Network{
		dnsNetwork(config.DHCPOff, rng("10.0.0.50", "10.0.0.60"), config.Range{}),
		dnsNetwork(config.DHCPFull, config.Range{}, config.Range{}),
	} {
		t.Run(string(n.DHCPMode()), func(t *testing.T) {
			instances := []config.Instance{{Name: "dns", Network: "lan", StaticIP: netip.MustParseAddr("10.0.0.53")}}
			if err := Assign([]config.Network{n}, instances, nil); err != nil {
				t.Fatalf("Assign(static_ip on the dns address): %v", err)
			}
			unpinned := []config.Instance{{Name: "dns", Network: "lan"}}
			if err := Assign([]config.Network{n}, unpinned, instances); err != nil {
				t.Fatalf("Assign(unpinned): %v", err)
			}
			if unpinned[0].StaticIP == netip.MustParseAddr("10.0.0.53") {
				t.Errorf("unpinned instance kept the carved dns address %v", unpinned[0].StaticIP)
			}
		})
	}
}

// TestNewNetworkPoolRejectsBadRanges: Allocation's issues reach IPAM too.
func TestNewNetworkPoolRejectsBadRanges(t *testing.T) {
	if _, err := NewNetworkPool(dnsNetwork(config.DHCPOff, rng("10.0.0.2", "10.0.0.100"), rng("10.0.0.100", "10.0.0.200"))); err == nil {
		t.Error("NewNetworkPool(overlapping ranges) succeeded, want an error")
	}
}

// TestReservations: a network's Instances after Assign become reservations,
// whether their address was explicit or auto-assigned (in full, from the
// pool), skipping one without a mac; an Instance with a mac and no address
// is an error.
func TestReservations(t *testing.T) {
	networks := []config.Network{
		dnsNetwork(config.DHCPFull, config.Range{}, rng("10.0.0.100", "10.0.0.200")),
		{Name: "other", CIDR: netip.MustParsePrefix("10.9.0.0/24"), DHCPExcludedRange: rng("10.9.0.10", "10.9.0.20")},
	}
	instances := []config.Instance{
		{Name: "node0", Network: "lan", MAC: "02:00:00:00:00:0A", StaticIP: netip.MustParseAddr("10.0.0.150")},
		{Name: "node1", Network: "lan", MAC: "02-00-00-00-00-0b"},
		{Name: "elsewhere", Network: "other", MAC: "02:00:00:00:00:0c"},
		{Name: "nomac", Network: "lan"},
	}
	if err := Assign(networks, instances, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	got, err := Reservations("lan", instances)
	if err != nil {
		t.Fatalf("Reservations: %v", err)
	}
	want := []Reservation{
		{MAC: "02:00:00:00:00:0a", IP: netip.MustParseAddr("10.0.0.150"), Hostname: "node0"},
		{MAC: "02:00:00:00:00:0b", IP: netip.MustParseAddr("10.0.0.100"), Hostname: "node1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Reservations = %+v, want %+v", got, want)
	}

	unassigned := []config.Instance{{Name: "node2", Network: "lan", MAC: "02:00:00:00:00:0d"}}
	if _, err := Reservations("lan", unassigned); !errors.Is(err, ErrUnassigned) {
		t.Errorf("Reservations(no address) = %v, want ErrUnassigned", err)
	}
	// Reservations gets Instances that Parse checked, but checks again: an
	// EUI-64 address would make a reservation no Ethernet NIC matches.
	eui64 := []config.Instance{{Name: "node3", Network: "lan", MAC: "02:00:00:00:00:00:00:01", StaticIP: netip.MustParseAddr("10.0.0.151")}}
	if _, err := Reservations("lan", eui64); err == nil {
		t.Error("Reservations(EUI-64 mac) = nil error, want one")
	}
	if got, err := Reservations("lan", []config.Instance{{Name: "nomac", Network: "lan"}}); err != nil || got != nil {
		t.Errorf("Reservations(no mac, no address) = %v, %v, want none and no error", got, err)
	}
}
