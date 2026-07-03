package config

import (
	"net/netip"
	"strings"
	"testing"
)

// validNetwork is a fully-populated, semantically-valid Network used as the
// baseline that the per-field tests below mutate.
func validNetwork() Network {
	return Network{
		Name:              "home-lan",
		CIDR:              netip.MustParsePrefix("192.168.1.0/24"),
		Gateway:           netip.MustParseAddr("192.168.1.1"),
		DHCPExcludedRange: Range{Start: netip.MustParseAddr("192.168.1.200"), End: netip.MustParseAddr("192.168.1.250")},
		DNS:               []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	}
}

func TestValidateAcceptsGoodConfig(t *testing.T) {
	cfg := Config{
		Networks: []Network{validNetwork()},
		Instances: []Instance{{
			Name: "node0", Network: "home-lan", StaticIP: netip.MustParseAddr("192.168.1.201"),
		}},
	}
	if issues := Validate(cfg); !issues.Empty() {
		t.Fatalf("Validate() = %v, want no issues", issues)
	}
}

func TestValidateAcceptsDHCPInstanceAndOptionalFields(t *testing.T) {
	// A DHCP network needs no gateway/range, and an instance with no static_ip
	// is a DHCP node — neither should be flagged.
	cfg := Config{
		Networks:  []Network{{Name: "dhcp-lan", CIDR: netip.MustParsePrefix("10.0.0.0/24")}},
		Instances: []Instance{{Name: "node0", Network: "dhcp-lan"}},
	}
	if issues := Validate(cfg); !issues.Empty() {
		t.Fatalf("Validate() = %v, want no issues", issues)
	}
}

func TestValidateNetworkIssues(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Network)
		wantPath string
	}{
		{"empty name", func(n *Network) { n.Name = "" }, "networks[0].name"},
		{"whitespace name", func(n *Network) { n.Name = "  " }, "networks[0].name"},
		{"missing cidr", func(n *Network) { n.CIDR = netip.Prefix{} }, "networks[0].cidr"},
		{"gateway outside cidr", func(n *Network) { n.Gateway = netip.MustParseAddr("10.0.0.1") }, "networks[0].gateway"},
		{"invalid dns entry", func(n *Network) { n.DNS = []netip.Addr{{}} }, "networks[0].dns[0]"},
		{"range outside cidr", func(n *Network) {
			n.DHCPExcludedRange = Range{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.9")}
		}, "networks[0].dhcp_excluded_range"},
		{"range start after end", func(n *Network) {
			n.DHCPExcludedRange = Range{Start: netip.MustParseAddr("192.168.1.250"), End: netip.MustParseAddr("192.168.1.200")}
		}, "networks[0].dhcp_excluded_range"},
		{"half-open range", func(n *Network) {
			n.DHCPExcludedRange = Range{Start: netip.MustParseAddr("192.168.1.200")}
		}, "networks[0].dhcp_excluded_range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := validNetwork()
			tc.mutate(&n)
			issues := Validate(Config{Networks: []Network{n}})
			if !hasPath(issues, tc.wantPath) {
				t.Fatalf("Validate() = %v, want an issue at %q", issues, tc.wantPath)
			}
		})
	}
}

func TestValidateRejectsDuplicateNetworkName(t *testing.T) {
	n := validNetwork()
	dup := validNetwork()
	dup.CIDR = netip.MustParsePrefix("10.0.0.0/24")
	dup.Gateway = netip.Addr{}
	dup.DHCPExcludedRange = Range{}

	issues := Validate(Config{Networks: []Network{n, dup}})
	if !hasPath(issues, "networks[1].name") {
		t.Fatalf("Validate() = %v, want a duplicate-name issue at networks[1].name", issues)
	}
}

func TestValidateDoesNotDoubleReportDuplicateEmptyNames(t *testing.T) {
	// Two networks with empty names are each already flagged as "must not be
	// empty" — they shouldn't also collide with each other as duplicates.
	n := validNetwork()
	n.Name = ""
	other := validNetwork()
	other.Name = ""
	other.CIDR = netip.MustParsePrefix("10.0.0.0/24")
	other.Gateway = netip.Addr{}
	other.DHCPExcludedRange = Range{}

	issues := Validate(Config{Networks: []Network{n, other}})
	for _, i := range issues {
		if strings.Contains(i.Message, "already defined by") {
			t.Fatalf("Validate() = %v, want no duplicate-name issue for empty names", issues)
		}
	}
}

func TestValidateInstanceIssues(t *testing.T) {
	net := validNetwork()
	tests := []struct {
		name     string
		inst     Instance
		wantPath string
	}{
		{"static_ip outside cidr",
			Instance{Name: "n", Network: "home-lan", StaticIP: netip.MustParseAddr("10.0.0.5")},
			"instances[0].static_ip"},
		{"static_ip outside excluded range",
			Instance{Name: "n", Network: "home-lan", StaticIP: netip.MustParseAddr("192.168.1.50")},
			"instances[0].static_ip"},
		{"unknown network",
			Instance{Name: "n", Network: "nope", StaticIP: netip.MustParseAddr("192.168.1.201")},
			"instances[0].network"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(Config{Networks: []Network{net}, Instances: []Instance{tc.inst}})
			if !hasPath(issues, tc.wantPath) {
				t.Fatalf("Validate() = %v, want an issue at %q", issues, tc.wantPath)
			}
		})
	}
}

func TestValidateReportsAllIssues(t *testing.T) {
	// Two independent problems must both surface, not just the first.
	n := validNetwork()
	n.Name = ""
	n.Gateway = netip.MustParseAddr("10.0.0.1")
	issues := Validate(Config{Networks: []Network{n}})
	if len(issues) != 2 {
		t.Fatalf("Validate() returned %d issues, want 2: %v", len(issues), issues)
	}
}

func TestIssuesErrorMentionsPath(t *testing.T) {
	issues := Validate(Config{Networks: []Network{{Name: ""}}})
	if msg := issues.Error(); !strings.Contains(msg, "networks[0]") {
		t.Fatalf("Issues.Error() = %q, want it to mention the path", msg)
	}
}

func hasPath(issues Issues, path string) bool {
	for _, i := range issues {
		if i.Path == path {
			return true
		}
	}
	return false
}
