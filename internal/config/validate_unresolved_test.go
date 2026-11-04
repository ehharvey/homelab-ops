package config

import (
	"net/netip"
	"testing"
)

// TestValidateUnresolved covers the agent's view of a synced fleet
// (docs/Decisions.md §28, #199): a state-addressed Network, a name only,
// passes; partial addressing and a git static_ip on a state-addressed
// network don't; and Validate, which wants resolved Networks, still rejects
// the name-only one.
func TestValidateUnresolved(t *testing.T) {
	stateNet := Network{Name: "lab"}
	tests := []struct {
		name     string
		cfg      Config
		path     string // "" means no issues
		contains string
	}{
		{
			name: "a state-addressed network passes",
			cfg: Config{
				Networks:  []Network{validNetwork(), stateNet},
				Instances: []Instance{{Name: "node0", Network: "lab"}},
			},
		},
		{
			name: "partial addressing names the network",
			cfg: Config{Networks: []Network{{
				Name: "lab", Gateway: netip.MustParseAddr("10.0.0.1"),
				DHCPExcludedRange: Range{Start: netip.MustParseAddr("10.0.0.200"), End: netip.MustParseAddr("10.0.0.250")},
			}}},
			path:     "networks[0]",
			contains: `network "lab" sets gateway, dhcp_excluded_range but no cidr`,
		},
		{
			name: "dns alone is partial",
			cfg:  Config{Networks: []Network{{Name: "lab", DNS: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}}},
			path: "networks[0]", contains: `network "lab" sets dns but no cidr`,
		},
		{
			name: "a static_ip on a state-addressed network names it",
			cfg: Config{
				Networks:  []Network{stateNet},
				Instances: []Instance{{Name: "node0", Network: "lab", StaticIP: netip.MustParseAddr("10.0.0.201")}},
			},
			path:     "instances[0].static_ip",
			contains: `network "lab" takes its addressing from the web app's state`,
		},
		{
			name: "a git-addressed network still gets the address checks",
			cfg: Config{Networks: []Network{{
				Name: "home", CIDR: netip.MustParsePrefix("192.168.1.0/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
			}}},
			path: "networks[0].gateway", contains: "is not inside cidr",
		},
		{
			name: "an unknown network is still caught",
			cfg:  Config{Networks: []Network{stateNet}, Instances: []Instance{{Name: "node0", Network: "nope"}}},
			path: "instances[0].network", contains: `unknown network "nope"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateUnresolved(tc.cfg)
			if tc.path == "" {
				if !issues.Empty() {
					t.Fatalf("ValidateUnresolved = %v, want none", issues)
				}
				return
			}
			if !issueContains(issues, tc.path, tc.contains) {
				t.Errorf("ValidateUnresolved = %v, want %s: …%s…", issues, tc.path, tc.contains)
			}
			if len(issues) != 1 {
				t.Errorf("ValidateUnresolved reported %d issues, want 1: %v", len(issues), issues)
			}
		})
	}

	// Validate wants every Network resolved, so render-seed and the web
	// app's post-resolve check keep rejecting a name-only Network.
	if issues := Validate(Config{Networks: []Network{stateNet}}); !hasPath(issues, "networks[0].cidr") {
		t.Errorf("Validate(name-only network) = %v, want a networks[0].cidr issue", issues)
	}
}

func TestAddressingFields(t *testing.T) {
	if got := validNetwork().AddressingFields(); len(got) != 4 {
		t.Errorf("AddressingFields(full) = %v, want all four", got)
	}
	if !(Network{Name: "lab"}).Unaddressed() {
		t.Error("a name-only Network should be Unaddressed")
	}
	if (Network{Name: "lab", DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}).Unaddressed() {
		t.Error("a Network with dns set is not Unaddressed")
	}
}
