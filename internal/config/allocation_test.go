package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func rng(start, end string) Range {
	return Range{Start: netip.MustParseAddr(start), End: netip.MustParseAddr(end)}
}

func rngs(pairs ...string) []Range {
	out := []Range{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, rng(pairs[i], pairs[i+1]))
	}
	return out
}

// TestParseDHCPMode: all four modes parse, an omitted one defaults to off
// through DHCPMode, and an unknown one is a parse error.
func TestParseDHCPMode(t *testing.T) {
	for _, mode := range []DHCPMode{DHCPOff, DHCPProxy, DHCPReservations, DHCPFull} {
		cfg, err := Parse(strings.NewReader("kind: Network\nname: lan\ncidr: 10.0.0.0/24\ndhcp:\n  mode: " + string(mode) + "\n  pool: 10.0.0.100-10.0.0.199\n"))
		if err != nil {
			t.Fatalf("Parse(mode %s): %v", mode, err)
		}
		n := cfg.Networks[0]
		if n.DHCP.Mode != mode || n.DHCPMode() != mode {
			t.Errorf("mode %s: DHCP.Mode = %q, DHCPMode() = %q", mode, n.DHCP.Mode, n.DHCPMode())
		}
		if want := rng("10.0.0.100", "10.0.0.199"); n.DHCP.Pool != want {
			t.Errorf("mode %s: DHCP.Pool = %v, want %v", mode, n.DHCP.Pool, want)
		}
	}

	cfg, err := Parse(strings.NewReader("kind: Network\nname: lan\ncidr: 10.0.0.0/24\n"))
	if err != nil {
		t.Fatalf("Parse(no dhcp): %v", err)
	}
	if n := cfg.Networks[0]; n.DHCP.Mode != "" || n.DHCPMode() != DHCPOff {
		t.Errorf("omitted mode: DHCP.Mode = %q, DHCPMode() = %q, want raw empty and off", n.DHCP.Mode, n.DHCPMode())
	}

	for _, bad := range []string{"dhcp:\n  mode: kea\n", "dhcp:\n  mode: Full\n", "dhcp:\n  pool: 10.0.0.1\n", "dhcp:\n  boot: x\n"} {
		if _, err := Parse(strings.NewReader("kind: Network\nname: lan\ncidr: 10.0.0.0/24\n" + bad)); err == nil {
			t.Errorf("Parse(%q) succeeded, want a parse error", bad)
		}
	}
}

// TestAllocation covers the per-mode derivation and the carving: written,
// derived (including a two-span complement), full with neither field
// written, and carving around the network, broadcast, gateway and in-cidr
// dns addresses.
func TestAllocation(t *testing.T) {
	base := func(mode DHCPMode) Network {
		return Network{
			Name:    "lan",
			CIDR:    netip.MustParsePrefix("10.0.0.0/24"),
			Gateway: netip.MustParseAddr("10.0.0.1"),
			DNS:     []netip.Addr{netip.MustParseAddr("10.0.0.53"), netip.MustParseAddr("1.1.1.1")},
			DHCP:    DHCP{Mode: mode},
		}
	}
	tests := []struct {
		name          string
		n             func() Network
		static, pool  Span
		nodesArePool  bool
		wantCarvedLen int
	}{
		{
			name:   "off, neither written: no ranges",
			n:      func() Network { return base("") },
			static: Span{Field: "dhcp_excluded_range", Source: SpanUnset, Effective: []Range{}},
			pool:   Span{Field: "dhcp.pool", Source: SpanUnset, Effective: []Range{}},
		},
		{
			name:   "reservations, neither written: no ranges",
			n:      func() Network { return base(DHCPReservations) },
			static: Span{Field: "dhcp_excluded_range", Source: SpanUnset, Effective: []Range{}},
			pool:   Span{Field: "dhcp.pool", Source: SpanUnset, Effective: []Range{}},
		},
		{
			name:   "full, neither written: the pool is the whole cidr, carved",
			n:      func() Network { return base(DHCPFull) },
			static: Span{Field: "dhcp_excluded_range", Source: SpanUnset, Effective: []Range{}},
			pool: Span{Field: "dhcp.pool", Source: SpanWholeCIDR,
				Uncarved:  rngs("10.0.0.0", "10.0.0.255"),
				Effective: rngs("10.0.0.2", "10.0.0.52", "10.0.0.54", "10.0.0.254")},
			nodesArePool: true,
		},
		{
			name: "off, static written mid-cidr: the pool is its two-span complement",
			n: func() Network {
				n := base(DHCPOff)
				n.DHCPExcludedRange = rng("10.0.0.100", "10.0.0.149")
				return n
			},
			static: Span{Field: "dhcp_excluded_range", Source: SpanWritten,
				Uncarved: rngs("10.0.0.100", "10.0.0.149"), Effective: rngs("10.0.0.100", "10.0.0.149")},
			pool: Span{Field: "dhcp.pool", Source: SpanComplement,
				Uncarved:  rngs("10.0.0.0", "10.0.0.99", "10.0.0.150", "10.0.0.255"),
				Effective: rngs("10.0.0.2", "10.0.0.52", "10.0.0.54", "10.0.0.99", "10.0.0.150", "10.0.0.254")},
		},
		{
			name: "full, pool written at the top: the static range is the one span below",
			n: func() Network {
				n := base(DHCPFull)
				n.DHCP.Pool = rng("10.0.0.100", "10.0.0.255")
				return n
			},
			static: Span{Field: "dhcp_excluded_range", Source: SpanComplement,
				Uncarved: rngs("10.0.0.0", "10.0.0.99"), Effective: rngs("10.0.0.2", "10.0.0.52", "10.0.0.54", "10.0.0.99")},
			pool: Span{Field: "dhcp.pool", Source: SpanWritten,
				Uncarved: rngs("10.0.0.100", "10.0.0.255"), Effective: rngs("10.0.0.100", "10.0.0.254")},
			nodesArePool: true,
		},
		{
			name: "proxy, both written, the static range holding the gateway and dns",
			n: func() Network {
				n := base(DHCPProxy)
				n.DHCPExcludedRange = rng("10.0.0.1", "10.0.0.60")
				n.DHCP.Pool = rng("10.0.0.61", "10.0.0.200")
				return n
			},
			static: Span{Field: "dhcp_excluded_range", Source: SpanWritten,
				Uncarved: rngs("10.0.0.1", "10.0.0.60"), Effective: rngs("10.0.0.2", "10.0.0.52", "10.0.0.54", "10.0.0.60")},
			pool: Span{Field: "dhcp.pool", Source: SpanWritten,
				Uncarved: rngs("10.0.0.61", "10.0.0.200"), Effective: rngs("10.0.0.61", "10.0.0.200")},
		},
		{
			name: "off, static range covers the whole cidr: the derived pool is empty",
			n: func() Network {
				n := base(DHCPOff)
				n.DHCPExcludedRange = rng("10.0.0.0", "10.0.0.255")
				return n
			},
			static: Span{Field: "dhcp_excluded_range", Source: SpanWritten,
				Uncarved: rngs("10.0.0.0", "10.0.0.255"), Effective: rngs("10.0.0.2", "10.0.0.52", "10.0.0.54", "10.0.0.254")},
			pool: Span{Field: "dhcp.pool", Source: SpanComplement, Uncarved: []Range{}, Effective: []Range{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.n()
			a, issues := n.Allocation()
			if !issues.Empty() {
				t.Fatalf("Allocation: %v", issues)
			}
			if a.Mode != n.DHCPMode() {
				t.Errorf("Mode = %q, want %q", a.Mode, n.DHCPMode())
			}
			if !reflect.DeepEqual(a.Static, tc.static) {
				t.Errorf("Static = %+v,\nwant %+v", a.Static, tc.static)
			}
			if !reflect.DeepEqual(a.Pool, tc.pool) {
				t.Errorf("Pool = %+v,\nwant %+v", a.Pool, tc.pool)
			}
			if got := a.Nodes().Field == "dhcp.pool"; got != tc.nodesArePool {
				t.Errorf("Nodes() is the pool = %v, want %v", got, tc.nodesArePool)
			}
			want := []netip.Addr{netip.MustParseAddr("10.0.0.0"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.53"), netip.MustParseAddr("10.0.0.255")}
			if !reflect.DeepEqual(a.Carved, want) {
				t.Errorf("Carved = %v, want %v (1.1.1.1 is outside the cidr)", a.Carved, want)
			}
		})
	}
}

// TestAllocationIssues: a written field outside the cidr, reversed, or the
// two overlapping, each at its field's relative path.
func TestAllocationIssues(t *testing.T) {
	tests := []struct {
		name          string
		static, pool  Range
		path, message string
	}{
		{"static outside the cidr", rng("10.0.1.1", "10.0.1.9"), Range{}, "dhcp_excluded_range", "is not contained in cidr"},
		{"pool outside the cidr", Range{}, rng("10.0.0.200", "10.0.1.9"), "dhcp.pool", "is not contained in cidr"},
		{"pool reversed", Range{}, rng("10.0.0.200", "10.0.0.100"), "dhcp.pool", "is after end"},
		{"overlap", rng("10.0.0.2", "10.0.0.100"), rng("10.0.0.100", "10.0.0.200"), "dhcp.pool", "overlaps dhcp_excluded_range 10.0.0.2-10.0.0.100"},
		{"pool inside static", rng("10.0.0.2", "10.0.0.200"), rng("10.0.0.50", "10.0.0.60"), "dhcp.pool", "overlaps"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := Network{Name: "lan", CIDR: netip.MustParsePrefix("10.0.0.0/24"), DHCPExcludedRange: tc.static, DHCP: DHCP{Mode: DHCPFull, Pool: tc.pool}}
			_, issues := n.Allocation()
			if len(issues) != 1 || issues[0].Path != tc.path || !strings.Contains(issues[0].Message, tc.message) {
				t.Errorf("issues = %v, want one at %s containing %q", issues, tc.path, tc.message)
			}
		})
	}
	if _, issues := (Network{Name: "lan"}).Allocation(); len(issues) != 1 || issues[0].Path != "cidr" {
		t.Errorf("no cidr: issues = %v, want one at cidr", issues)
	}
}

// TestValidateDHCPRanges: the per-mode static_ip rules, mode by mode, and
// the range issues reported at the network's paths.
func TestValidateDHCPRanges(t *testing.T) {
	network := func(mode DHCPMode, static, pool Range) Network {
		return Network{
			Name:              "lan",
			CIDR:              netip.MustParsePrefix("10.0.0.0/24"),
			Gateway:           netip.MustParseAddr("10.0.0.1"),
			DNS:               []netip.Addr{netip.MustParseAddr("10.0.0.53"), netip.MustParseAddr("1.1.1.1")},
			DHCPExcludedRange: static,
			DHCP:              DHCP{Mode: mode, Pool: pool},
		}
	}
	none := Range{}
	tests := []struct {
		name     string
		n        Network
		staticIP string
		wantPath string // "" means valid
		wantMsg  string
	}{
		{"off: in the written static range", network("", rng("10.0.0.40", "10.0.0.60"), none), "10.0.0.41", "", ""},
		{"off: an in-cidr dns address in the static range is accepted", network(DHCPOff, rng("10.0.0.40", "10.0.0.60"), none), "10.0.0.53", "", ""},
		{"off: outside the written static range", network(DHCPOff, rng("10.0.0.40", "10.0.0.60"), none), "10.0.0.70", "instances[0].static_ip", "is outside dhcp_excluded_range 10.0.0.40-10.0.0.60"},
		{"off: neither written, anywhere in the cidr", network(DHCPOff, none, none), "10.0.0.200", "", ""},
		{"proxy: inside the written pool is outside the derived static range", network(DHCPProxy, none, rng("10.0.0.100", "10.0.0.199")), "10.0.0.150", "instances[0].static_ip", "outside dhcp_excluded_range 10.0.0.0-10.0.0.99, 10.0.0.200-10.0.0.255 (derived: the cidr minus dhcp.pool)"},
		{"proxy: in the derived static range's upper span", network(DHCPProxy, none, rng("10.0.0.100", "10.0.0.199")), "10.0.0.210", "", ""},
		{"reservations: outside the written static range", network(DHCPReservations, rng("10.0.0.2", "10.0.0.99"), rng("10.0.0.100", "10.0.0.254")), "10.0.0.120", "instances[0].static_ip", "is outside dhcp_excluded_range 10.0.0.2-10.0.0.99"},
		{"full: neither written, in the whole-cidr pool", network(DHCPFull, none, none), "10.0.0.10", "", ""},
		{"full: neither written, the dns address is accepted", network(DHCPFull, none, none), "10.0.0.53", "", ""},
		{"full: in the static range is outside the pool", network(DHCPFull, rng("10.0.0.2", "10.0.0.99"), rng("10.0.0.100", "10.0.0.254")), "10.0.0.20", "instances[0].static_ip", "is outside dhcp.pool 10.0.0.100-10.0.0.254"},
		{"full: outside the derived pool", network(DHCPFull, rng("10.0.0.2", "10.0.0.99"), none), "10.0.0.20", "instances[0].static_ip", "outside dhcp.pool 10.0.0.0-10.0.0.1, 10.0.0.100-10.0.0.255 (derived: the cidr minus dhcp_excluded_range)"},
		{"full: in the derived pool", network(DHCPFull, rng("10.0.0.2", "10.0.0.99"), none), "10.0.0.120", "", ""},
		{"full: the gateway is still a collision", network(DHCPFull, none, none), "10.0.0.1", "instances[0].static_ip", "collides with the gateway"},
		{"pool outside the cidr", network(DHCPFull, none, rng("10.0.0.100", "10.0.1.10")), "10.0.0.10", "networks[0].dhcp.pool", "is not contained in cidr"},
		{"pool overlapping the static range", network(DHCPOff, rng("10.0.0.2", "10.0.0.120"), rng("10.0.0.100", "10.0.0.200")), "10.0.0.10", "networks[0].dhcp.pool", "overlaps dhcp_excluded_range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{
				Networks:  []Network{tc.n},
				Instances: []Instance{{Name: "node0", MAC: "02:00:00:00:00:01", Network: "lan", StaticIP: netip.MustParseAddr(tc.staticIP)}},
			}
			issues := Validate(c)
			if tc.wantPath == "" {
				if !issues.Empty() {
					t.Errorf("Validate: %v, want none", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].Path != tc.wantPath || !strings.Contains(issues[0].Message, tc.wantMsg) {
				t.Errorf("Validate = %v, want one issue at %s containing %q", issues, tc.wantPath, tc.wantMsg)
			}
		})
	}
}

// TestParseRejectsMalformedMAC: a mac is syntax-checked at parse in every
// mode, off included. EUI-64 and InfiniBand addresses, which net.ParseMAC
// accepts, are rejected: they aren't Ethernet MACs.
func TestParseRejectsMalformedMAC(t *testing.T) {
	const network = "kind: Network\nname: lan\ncidr: 10.0.0.0/24\n---\nkind: Instance\nname: node0\nnetwork: lan\n"
	eui64 := "02:00:00:00:00:00:00:01"
	infiniband := "00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01"
	for _, mac := range []string{"02:00:00:00:00", "zz:00:00:00:00:01", "not-a-mac", eui64, "0200.0000.0000.0001", infiniband} {
		_, err := Parse(strings.NewReader(network + "mac: " + mac + "\n"))
		if err == nil || !strings.Contains(err.Error(), "mac") {
			t.Errorf("Parse(mac %q) = %v, want a mac parse error", mac, err)
		}
	}
	for _, mac := range []string{"02:00:00:00:00:01", "02-00-00-00-00-01", "AA:BB:CC:DD:EE:FF", "\"\""} {
		if _, err := Parse(strings.NewReader(network + "mac: " + mac + "\n")); err != nil {
			t.Errorf("Parse(mac %s): %v", mac, err)
		}
	}
}

// TestValidateMAC: a mac ParseMAC rejects is an error on any network (Validate
// also sees Configs Parse never checked); a missing mac is an error on
// reservations and full networks only, and a duplicate one, compared
// normalised, within one such network only.
func TestValidateMAC(t *testing.T) {
	networks := []Network{
		{Name: "off", CIDR: netip.MustParsePrefix("10.0.0.0/24")},
		{Name: "proxy", CIDR: netip.MustParsePrefix("10.0.1.0/24"), DHCP: DHCP{Mode: DHCPProxy}},
		{Name: "resv", CIDR: netip.MustParsePrefix("10.0.2.0/24"), DHCP: DHCP{Mode: DHCPReservations}},
		{Name: "full", CIDR: netip.MustParsePrefix("10.0.3.0/24"), DHCP: DHCP{Mode: DHCPFull}},
		{Name: "full2", CIDR: netip.MustParsePrefix("10.0.4.0/24"), DHCP: DHCP{Mode: DHCPFull}},
	}
	inst := func(name, network, mac string) Instance { return Instance{Name: name, Network: network, MAC: mac} }
	tests := []struct {
		name      string
		instances []Instance
		want      []string // issue paths
	}{
		{"missing on off and proxy is fine", []Instance{inst("a", "off", ""), inst("b", "proxy", "")}, nil},
		{"missing on reservations", []Instance{inst("a", "resv", "")}, []string{"instances[0].mac"}},
		{"missing on full", []Instance{inst("a", "full", "")}, []string{"instances[0].mac"}},
		{"duplicate on off is not checked", []Instance{inst("a", "off", "02:00:00:00:00:01"), inst("b", "off", "02:00:00:00:00:01")}, nil},
		{"duplicate on reservations", []Instance{inst("a", "resv", "02:00:00:00:00:01"), inst("b", "resv", "02:00:00:00:00:01")}, []string{"instances[1].mac"}},
		{"duplicate on full, compared normalised", []Instance{inst("a", "full", "AA:BB:CC:DD:EE:01"), inst("b", "full", "aa-bb-cc-dd-ee-01")}, []string{"instances[1].mac"}},
		{"an EUI-64 mac on off", []Instance{inst("a", "off", "02:00:00:00:00:00:00:01")}, []string{"instances[0].mac"}},
		{"an EUI-64 mac on full, rejected rather than compared", []Instance{inst("a", "full", "02:00:00:00:00:00:00:01"), inst("b", "full", "02-00-00-00-00-00-00-01")}, []string{"instances[0].mac", "instances[1].mac"}},
		{"a malformed mac on reservations", []Instance{inst("a", "resv", "not-a-mac")}, []string{"instances[0].mac"}},
		{"the same mac on two full networks", []Instance{inst("a", "full", "02:00:00:00:00:01"), inst("b", "full2", "02:00:00:00:00:01")}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, is := range Validate(Config{Networks: networks, Instances: tc.instances}) {
				got = append(got, is.Path)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("issue paths = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAddressingFieldsDHCPPool: dhcp.pool is addressing, so a Network that
// sets it without a cidr is partial, while dhcp.mode alone leaves it
// unaddressed. WithAddressingOf copies the pool but keeps the mode.
func TestAddressingFieldsDHCPPool(t *testing.T) {
	pool := rng("10.0.0.100", "10.0.0.199")
	if got := (Network{Name: "lan", DHCP: DHCP{Mode: DHCPFull}}); !got.Unaddressed() {
		t.Errorf("a Network with only dhcp.mode: AddressingFields = %v, want none", got.AddressingFields())
	}
	partial := Config{Networks: []Network{{Name: "lan", DHCP: DHCP{Pool: pool}}}}
	issues := AddressingIssues(partial)
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "sets dhcp.pool but no cidr") {
		t.Errorf("AddressingIssues(pool without cidr) = %v, want it partial", issues)
	}
	full := Network{Name: "lan", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
		DHCPExcludedRange: rng("10.0.0.2", "10.0.0.9"), DNS: []netip.Addr{netip.MustParseAddr("10.0.0.1")}, DHCP: DHCP{Pool: pool}}
	if got, want := full.AddressingFields(), []string{"cidr", "gateway", "dhcp_excluded_range", "dns", "dhcp.pool"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AddressingFields = %v, want %v", got, want)
	}
	entry := full
	entry.Name, entry.DHCP.Mode = "entry", DHCPProxy
	joined := Network{Name: "git-name", DHCP: DHCP{Mode: DHCPFull}}.WithAddressingOf(entry)
	want := full
	want.Name, want.DHCP.Mode = "git-name", DHCPFull
	if !reflect.DeepEqual(joined, want) {
		t.Errorf("WithAddressingOf = %+v, want %+v", joined, want)
	}
}

func TestLastAddr(t *testing.T) {
	for _, tc := range []struct{ prefix, want string }{
		{"192.168.1.0/24", "192.168.1.255"},
		{"192.168.1.77/24", "192.168.1.255"}, // unmasked input
		{"10.0.0.0/23", "10.0.1.255"},
		{"10.0.0.32/27", "10.0.0.63"},
		{"10.0.0.5/32", "10.0.0.5"},
		{"0.0.0.0/0", "255.255.255.255"},
		{"fd00::/64", "fd00::ffff:ffff:ffff:ffff"},
		{"fd00::/61", "fd00::7:ffff:ffff:ffff:ffff"},
	} {
		if got := LastAddr(netip.MustParsePrefix(tc.prefix)); got.String() != tc.want {
			t.Errorf("LastAddr(%s) = %s, want %s", tc.prefix, got, tc.want)
		}
	}
}
