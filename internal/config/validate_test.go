package config

import (
	"net/netip"
	"reflect"
	"slices"
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

func TestValidateInstanceNameIssues(t *testing.T) {
	tests := []struct {
		name     string
		instName string
		wantMsg  string
	}{
		{"empty", "", "must not be empty"},
		{"uppercase", "Node0", "lowercase hostname label"},
		{"leading hyphen", "-node0", "lowercase hostname label"},
		{"trailing hyphen", "node0-", "lowercase hostname label"},
		{"all digits", "42", "lowercase hostname label"},
		{"underscore", "node_0", "lowercase hostname label"},
		{"dot", "node0.lan", "lowercase hostname label"},
		{"whitespace", " node0", "lowercase hostname label"},
		{"64 characters", strings.Repeat("a", 64), "lowercase hostname label"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Networks:  []Network{validNetwork()},
				Instances: []Instance{{Name: tc.instName, Network: "home-lan"}},
			}
			issues := Validate(cfg)
			if !issueContains(issues, "instances[0].name", tc.wantMsg) {
				t.Fatalf("Validate() = %v, want an issue at instances[0].name containing %q", issues, tc.wantMsg)
			}
		})
	}
}

func TestValidateAcceptsHostnameLabelInstanceNames(t *testing.T) {
	for _, name := range []string{"n", "node0", "node-0", "0node", "a1-b2-c3", strings.Repeat("a", 63)} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				Networks:  []Network{validNetwork()},
				Instances: []Instance{{Name: name, Network: "home-lan"}},
			}
			if issues := Validate(cfg); !issues.Empty() {
				t.Fatalf("Validate() = %v, want no issues", issues)
			}
		})
	}
}

func TestValidateRejectsDuplicateInstanceName(t *testing.T) {
	cfg := Config{
		Networks: []Network{validNetwork()},
		Instances: []Instance{
			{Name: "node0", Network: "home-lan"},
			{Name: "node0", Network: "home-lan"},
		},
	}
	issues := Validate(cfg)
	if !issueContains(issues, "instances[1].name", "already defined by instances[0]") {
		t.Fatalf("Validate() = %v, want a duplicate-name issue at instances[1].name", issues)
	}
}

func TestValidateDoesNotDoubleReportDuplicateEmptyInstanceNames(t *testing.T) {
	// Two empty names are each already "must not be empty"; they shouldn't
	// also collide with each other as duplicates.
	cfg := Config{
		Networks:  []Network{validNetwork()},
		Instances: []Instance{{Name: "", Network: "home-lan"}, {Name: "", Network: "home-lan"}},
	}
	for _, i := range Validate(cfg) {
		if strings.Contains(i.Message, "already defined by") {
			t.Fatalf("Validate() reported %v, want no duplicate-name issue for empty names", i)
		}
	}
}

func TestValidateChecksEveryInstanceNetwork(t *testing.T) {
	// A DHCP instance used to skip validation entirely, so an unknown network
	// only surfaced at IPAM or render time.
	dhcp := Validate(Config{Networks: []Network{validNetwork()}, Instances: []Instance{{Name: "n", Network: "nope"}}})
	if !issueContains(dhcp, "instances[0].network", `unknown network "nope"`) {
		t.Fatalf("Validate() = %v, want an unknown-network issue for a DHCP instance", dhcp)
	}

	// A static instance on an unknown network is reported once, not again by
	// the address checks that follow.
	static := Validate(Config{
		Networks:  []Network{validNetwork()},
		Instances: []Instance{{Name: "n", Network: "nope", StaticIP: netip.MustParseAddr("192.168.1.201")}},
	})
	if len(static) != 1 || static[0].Path != "instances[0].network" {
		t.Fatalf("Validate() = %v, want exactly one issue, at instances[0].network", static)
	}
}

func TestValidateRejectsStaticIPCollisions(t *testing.T) {
	net := validNetwork() // 192.168.1.0/24, gateway 192.168.1.1
	tests := []struct {
		name    string
		ip      string
		wantMsg string
	}{
		{"collides with gateway", "192.168.1.1", "collides with the gateway"},
		{"collides with network address", "192.168.1.0", "collides with the network address"},
		{"collides with broadcast address", "192.168.1.255", "collides with the broadcast address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := Instance{Name: "n", Network: "home-lan", StaticIP: netip.MustParseAddr(tc.ip)}
			issues := Validate(Config{Networks: []Network{net}, Instances: []Instance{inst}})
			if !issueContains(issues, "instances[0].static_ip", tc.wantMsg) {
				t.Fatalf("Validate() = %v, want an issue at instances[0].static_ip containing %q", issues, tc.wantMsg)
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

func TestValidateAcceptsIncusOS(t *testing.T) {
	for _, hostOS := range []string{"", OSIncusOS} {
		cfg := Config{
			Networks:  []Network{validNetwork()},
			Instances: []Instance{{Name: "node0", Network: "home-lan", OS: hostOS}},
		}
		if issues := Validate(cfg); !issues.Empty() {
			t.Errorf("os %q: Validate() = %v, want no issues", hostOS, issues)
		}
	}
}

// TestValidateHostOSAndRuntime covers one Instance's os and runtime: every
// allowed-pairs row, allowed and disallowed, for both runtimes, and the check
// order that gives each Instance the one error naming its fix. An unknown os
// or runtime reports only itself; a disallowed pair reports only the pair,
// even when its os or runtime also has no backend; an allowed pair goes on to
// the no-backend errors, os then runtime. The fleet has one Instance, so the
// one-cluster check can't add to it.
func TestValidateHostOSAndRuntime(t *testing.T) {
	const (
		unknownOS   = `is not a known host OS; known: incusos, talos, debian`
		talosNoOS   = `"talos" has no backend yet; implemented: incusos`
		debianNoOS  = `"debian" has no backend yet; implemented: incusos`
		k8sNoRT     = `"kubernetes" has no backend yet; implemented: incus`
		unknownRT   = `is not a known runtime; known: incus, kubernetes`
		omittedNote = ` (the default when runtime is omitted)`
	)
	type want struct{ field, msg string }
	tests := []struct {
		os, runtime string
		want        []want
	}{
		// incusos row: incus only.
		{"", "", nil},
		{OSIncusOS, "", nil},
		{OSIncusOS, RuntimeIncus, nil},
		{"", RuntimeIncus, nil},
		{OSIncusOS, RuntimeKubernetes, []want{{"runtime", `os "incusos" only runs incus, not "kubernetes"`}}},
		{"", RuntimeKubernetes, []want{{"runtime", `os "incusos" only runs incus, not "kubernetes"`}}},
		// talos row: kubernetes only. The disallowed pair is reported alone,
		// not "talos has no backend yet".
		{OSTalos, RuntimeIncus, []want{{"runtime", `os "talos" only runs kubernetes, not "incus"`}}},
		{OSTalos, "", []want{{"runtime", `os "talos" only runs kubernetes, not "incus"` + omittedNote}}},
		{OSTalos, RuntimeKubernetes, []want{{"os", talosNoOS}, {"runtime", k8sNoRT}}},
		// debian row: both runtimes, so neither is a pair error.
		{OSDebian, RuntimeIncus, []want{{"os", debianNoOS}}},
		{OSDebian, "", []want{{"os", debianNoOS}}},
		{OSDebian, RuntimeKubernetes, []want{{"os", debianNoOS}, {"runtime", k8sNoRT}}},
		// An unknown os reports only itself, whatever the runtime.
		{"windows", "", []want{{"os", `"windows" ` + unknownOS}}},
		{"IncusOS", "", []want{{"os", `"IncusOS" ` + unknownOS}}},
		{"windows", RuntimeKubernetes, []want{{"os", `"windows" ` + unknownOS}}},
		{"windows", "docker", []want{{"os", `"windows" ` + unknownOS}}},
		// An unknown runtime reports only itself, whatever the os.
		{OSIncusOS, "docker", []want{{"runtime", `"docker" ` + unknownRT}}},
		{OSTalos, "Kubernetes", []want{{"runtime", `"Kubernetes" ` + unknownRT}}},
		{OSDebian, "docker", []want{{"runtime", `"docker" ` + unknownRT}}},
	}
	for _, tc := range tests {
		t.Run(tc.os+"+"+tc.runtime, func(t *testing.T) {
			cfg := Config{
				Networks:  []Network{validNetwork()},
				Instances: []Instance{{Name: "node0", Network: "home-lan", OS: tc.os, Runtime: tc.runtime}},
			}
			var wantIssues Issues
			for _, w := range tc.want {
				wantIssues = append(wantIssues, Issue{Path: "instances[0]." + w.field, Message: w.msg})
			}
			if got := Validate(cfg); !reflect.DeepEqual(got, wantIssues) {
				t.Fatalf("Validate(os %q, runtime %q) = %#v, want %#v", tc.os, tc.runtime, got, wantIssues)
			}
		})
	}
}

// TestAllowedRuntimesCoversKnownNames pins the allowed-pairs table to the
// known names: every known host OS has a non-empty row, the table has no row
// for an unknown OS, and every row names only known runtimes. A new host OS
// that forgot its row would otherwise get a pair error naming no runtime.
func TestAllowedRuntimesCoversKnownNames(t *testing.T) {
	for _, hostOS := range hostOSes {
		if len(allowedRuntimes[hostOS]) == 0 {
			t.Errorf("host OS %q has no allowed-runtimes row", hostOS)
		}
	}
	for hostOS, rts := range allowedRuntimes {
		if !slices.Contains(hostOSes, hostOS) {
			t.Errorf("allowed-runtimes row for unknown host OS %q", hostOS)
		}
		for _, rt := range rts {
			if !slices.Contains(runtimes, rt) {
				t.Errorf("host OS %q's row names unknown runtime %q", hostOS, rt)
			}
		}
	}
}

// TestValidateOneClusterPerFleet covers the fleet-wide rule: every Instance is
// a member of the fleet's one cluster, so all must resolve to the same os and
// the same runtime. Resolved values are compared (omitted equals the
// default), and an unknown name, which has its own error, is left out.
func TestValidateOneClusterPerFleet(t *testing.T) {
	node := func(name, hostOS, rt string) Instance {
		return Instance{Name: name, Network: "home-lan", OS: hostOS, Runtime: rt}
	}
	tests := []struct {
		name      string
		instances []Instance
		want      Issues // the complete result, per-Instance issues included
	}{
		{
			name:      "omitted equals the default",
			instances: []Instance{node("node0", "", ""), node("node1", OSIncusOS, RuntimeIncus), node("node2", "", RuntimeIncus)},
		},
		{
			name:      "mixed runtime",
			instances: []Instance{node("node0", OSDebian, ""), node("node1", OSDebian, RuntimeKubernetes)},
			want: Issues{
				{Path: "instances[0].os", Message: `"debian" has no backend yet; implemented: incusos`},
				{Path: "instances[1].os", Message: `"debian" has no backend yet; implemented: incusos`},
				{Path: "instances[1].runtime", Message: `"kubernetes" has no backend yet; implemented: incus`},
				{Path: "instances[1].runtime", Message: `"kubernetes" differs from instances[0]'s "incus": a fleet is one cluster, and its members all run one runtime`},
			},
		},
		{
			name:      "mixed os",
			instances: []Instance{node("node0", "", ""), node("node1", OSDebian, RuntimeIncus)},
			want: Issues{
				{Path: "instances[1].os", Message: `"debian" has no backend yet; implemented: incusos`},
				{Path: "instances[1].os", Message: `"debian" differs from instances[0]'s "incusos": a fleet is one cluster, and its members all run one host OS`},
			},
		},
		{
			name:      "mixed os and runtime report both",
			instances: []Instance{node("node0", "", ""), node("node1", OSTalos, RuntimeKubernetes)},
			want: Issues{
				{Path: "instances[1].os", Message: `"talos" has no backend yet; implemented: incusos`},
				{Path: "instances[1].runtime", Message: `"kubernetes" has no backend yet; implemented: incus`},
				{Path: "instances[1].os", Message: `"talos" differs from instances[0]'s "incusos": a fleet is one cluster, and its members all run one host OS`},
				{Path: "instances[1].runtime", Message: `"kubernetes" differs from instances[0]'s "incus": a fleet is one cluster, and its members all run one runtime`},
			},
		},
		{
			name:      "unknown names are left out",
			instances: []Instance{node("node0", "windows", "docker"), node("node1", "", ""), node("node2", "", "docker")},
			want: Issues{
				{Path: "instances[0].os", Message: `"windows" is not a known host OS; known: incusos, talos, debian`},
				{Path: "instances[2].runtime", Message: `"docker" is not a known runtime; known: incus, kubernetes`},
			},
		},
		{
			name:      "compared against the first known value",
			instances: []Instance{node("node0", "windows", ""), node("node1", OSDebian, ""), node("node2", "", "")},
			want: Issues{
				{Path: "instances[0].os", Message: `"windows" is not a known host OS; known: incusos, talos, debian`},
				{Path: "instances[1].os", Message: `"debian" has no backend yet; implemented: incusos`},
				{Path: "instances[2].os", Message: `"incusos" differs from instances[1]'s "debian": a fleet is one cluster, and its members all run one host OS`},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Networks: []Network{validNetwork()}, Instances: tc.instances}
			if got := Validate(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Validate() =\n%#v\nwant\n%#v", got, tc.want)
			}
		})
	}
}

func TestClusterRuntimeDefault(t *testing.T) {
	if got := (Instance{}).ClusterRuntime(); got != DefaultRuntime || DefaultRuntime != RuntimeIncus {
		t.Errorf("ClusterRuntime() of an omitted runtime = %q, want %q", got, RuntimeIncus)
	}
	if got := (Instance{Runtime: RuntimeKubernetes}).ClusterRuntime(); got != RuntimeKubernetes {
		t.Errorf("ClusterRuntime() = %q, want %q", got, RuntimeKubernetes)
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

func issueContains(issues Issues, path, substr string) bool {
	for _, i := range issues {
		if i.Path == path && strings.Contains(i.Message, substr) {
			return true
		}
	}
	return false
}

// validApp is a fully-populated, semantically-valid App used as the baseline
// that the per-field tests below mutate.
func validApp() App {
	return App{
		Name:     "web-frontend",
		Type:     "some-renderer",
		Replicas: Replicas{Count: 1},
		Image:    ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "some/image:latest"},
	}
}

func TestValidateAcceptsGoodApp(t *testing.T) {
	if issues := Validate(Config{Apps: []App{validApp()}}); !issues.Empty() {
		t.Fatalf("Validate() = %v, want no issues", issues)
	}
}

func TestValidateAppIssues(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*App)
		wantPath string
	}{
		{"empty name", func(a *App) { a.Name = "" }, "apps[0].name"},
		{"whitespace name", func(a *App) { a.Name = "  " }, "apps[0].name"},
		{"empty type", func(a *App) { a.Type = "" }, "apps[0].type"},
		{"no alias or fingerprint", func(a *App) { a.Image = ImageRef{Server: "https://ghcr.io", Protocol: "oci"} }, "apps[0].image"},
		// replicas is required, so all three of these mean the same thing.
		{"replicas omitted", func(a *App) { a.Replicas = Replicas{} }, "apps[0].replicas"},
		{"replicas zero", func(a *App) { a.Replicas = Replicas{Count: 0} }, "apps[0].replicas"},
		{"replicas negative", func(a *App) { a.Replicas = Replicas{Count: -1} }, "apps[0].replicas"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := validApp()
			tc.mutate(&a)
			issues := Validate(Config{Apps: []App{a}})
			if !hasPath(issues, tc.wantPath) {
				t.Fatalf("Validate() = %v, want an issue at %q", issues, tc.wantPath)
			}
		})
	}
}

func TestValidateAcceptsFingerprintOnlyImage(t *testing.T) {
	a := validApp()
	a.Image = ImageRef{Fingerprint: "abc123"}
	if issues := Validate(Config{Apps: []App{a}}); !issues.Empty() {
		t.Fatalf("Validate() = %v, want no issues", issues)
	}
}

func TestValidateRejectsDuplicateAppName(t *testing.T) {
	cfg := Config{Apps: []App{validApp(), validApp()}}
	issues := Validate(cfg)
	if !issueContains(issues, "apps[1].name", "already defined by apps[0]") {
		t.Fatalf("Validate() = %v, want a duplicate-name issue at apps[1].name", issues)
	}
}

// An empty name is reported as empty, not additionally as a duplicate of the
// other empty one — mirroring the Network rule.
func TestValidateDoesNotDoubleReportDuplicateEmptyAppNames(t *testing.T) {
	a, b := validApp(), validApp()
	a.Name, b.Name = "", ""
	issues := Validate(Config{Apps: []App{a, b}})
	if issueContains(issues, "apps[1].name", "already defined") {
		t.Fatalf("Validate() = %v, want no duplicate-name issue for empty names", issues)
	}
}

// The rule the superseded "at most one kind: AgentConfig" singleton became:
// two per-node Apps of one type would race to manage the same node.
func TestValidateRejectsSecondPerNodeAppOfSameType(t *testing.T) {
	a, b := validApp(), validApp()
	a.Name, b.Name = "agent", "agent-2"
	a.Type, b.Type = "agent", "agent"
	a.Replicas, b.Replicas = Replicas{PerNode: true}, Replicas{PerNode: true}
	issues := Validate(Config{Apps: []App{a, b}})
	if !issueContains(issues, "apps[1].replicas", `a per-node app of type "agent" is already defined by apps[0]`) {
		t.Fatalf("Validate() = %v, want a duplicate per-node issue at apps[1].replicas", issues)
	}
}

// A missing `type` is reported once, as a missing type — not a second time as
// a duplicate per-node app of type "". Mirrors the empty-name rule above.
func TestValidateDoesNotDoubleReportPerNodeAppsWithEmptyType(t *testing.T) {
	a, b := validApp(), validApp()
	a.Name, b.Name = "agent", "alloy"
	a.Type, b.Type = "", ""
	a.Replicas, b.Replicas = Replicas{PerNode: true}, Replicas{PerNode: true}
	issues := Validate(Config{Apps: []App{a, b}})
	if !hasPath(issues, "apps[1].type") {
		t.Fatalf("Validate() = %v, want an issue at apps[1].type", issues)
	}
	if issueContains(issues, "apps[1].replicas", "already defined") {
		t.Errorf("Validate() = %v, want no duplicate per-node issue for an empty type", issues)
	}
}

// The per-node key is trimmed, so whitespace can't smuggle a second per-node
// app of the same renderer type past the check.
func TestValidateRejectsPerNodeDuplicateAcrossWhitespaceInType(t *testing.T) {
	a, b := validApp(), validApp()
	a.Name, b.Name = "agent", "agent-2"
	a.Type, b.Type = "agent", "  agent  "
	a.Replicas, b.Replicas = Replicas{PerNode: true}, Replicas{PerNode: true}
	issues := Validate(Config{Apps: []App{a, b}})
	if !issueContains(issues, "apps[1].replicas", "already defined by apps[0]") {
		t.Fatalf("Validate() = %v, want a duplicate per-node issue at apps[1].replicas", issues)
	}
}

// Per-node is a general shape, not an agent special case: the agent and Alloy
// both declare it, and they must coexist.
func TestValidateAcceptsPerNodeAppsOfDifferentTypes(t *testing.T) {
	a, b := validApp(), validApp()
	a.Name, b.Name = "agent", "alloy"
	a.Type, b.Type = "agent", "alloy"
	a.Replicas, b.Replicas = Replicas{PerNode: true}, Replicas{PerNode: true}
	if issues := Validate(Config{Apps: []App{a, b}}); !issues.Empty() {
		t.Fatalf("Validate() = %v, want no issues", issues)
	}
}
