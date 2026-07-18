package config

import (
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sampleFleet = `
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 192.168.1.200-192.168.1.250
dns: [192.168.1.1]
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: home-lan
static_ip: 192.168.1.201
disk: single
nic: single
security:
  tpm: false
  secure_boot: true
applications: [incus]
`

func TestParseSampleFleet(t *testing.T) {
	cfg, err := Parse(strings.NewReader(sampleFleet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cfg.Networks) != 1 {
		t.Fatalf("len(Networks) = %d, want 1", len(cfg.Networks))
	}
	wantNetwork := Network{
		Name:              "home-lan",
		CIDR:              netip.MustParsePrefix("192.168.1.0/24"),
		Gateway:           netip.MustParseAddr("192.168.1.1"),
		DHCPExcludedRange: Range{Start: netip.MustParseAddr("192.168.1.200"), End: netip.MustParseAddr("192.168.1.250")},
		DNS:               []netip.Addr{netip.MustParseAddr("192.168.1.1")},
	}
	if got := cfg.Networks[0]; !reflect.DeepEqual(got, wantNetwork) {
		t.Errorf("Networks[0] = %+v, want %+v", got, wantNetwork)
	}

	if len(cfg.Instances) != 1 {
		t.Fatalf("len(Instances) = %d, want 1", len(cfg.Instances))
	}
	wantInstance := Instance{
		Name:         "node0",
		MAC:          "aa:bb:cc:dd:ee:ff",
		Network:      "home-lan",
		StaticIP:     netip.MustParseAddr("192.168.1.201"),
		Disk:         "single",
		NIC:          "single",
		Security:     Security{TPM: false, SecureBoot: true},
		Applications: []string{"incus"},
	}
	if got := cfg.Instances[0]; !reflect.DeepEqual(got, wantInstance) {
		t.Errorf("Instances[0] = %+v, want %+v", got, wantInstance)
	}
}

// ExampleNetwork_yamlUnmarshal pins the third-party assumption the whole
// typed-config design rests on (see docs/Development Conventions.md § Config
// validation): yaml.v3 honors encoding.TextUnmarshaler, so the net/netip
// fields parse straight from YAML, and an empty (`gateway: ""`) or omitted
// (`dhcp_excluded_range`) optional lands as a zero value with IsValid()==false
// and no error. The Output block makes a future yaml.v3 bump that breaks this
// premise fail CI rather than silently rotting the docs.
func ExampleNetwork_yamlUnmarshal() {
	const doc = `
name: lan
cidr: 192.168.1.0/24
gateway: ""
dns: [1.1.1.1, 8.8.8.8]
`
	var n Network
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("cidr:", n.CIDR)                                   // netip.Prefix via TextUnmarshaler
	fmt.Println("dns:", n.DNS)                                     // []netip.Addr, each via TextUnmarshaler
	fmt.Println("gateway set:", n.Gateway.IsValid())               // empty string -> zero Addr, no error
	fmt.Println("range set:", n.DHCPExcludedRange.Start.IsValid()) // omitted -> zero Range, no error
	// Output:
	// cidr: 192.168.1.0/24
	// dns: [1.1.1.1 8.8.8.8]
	// gateway set: false
	// range set: false
}

func TestParseMultipleDocsOfSameKind(t *testing.T) {
	const fleet = `
kind: Network
name: home-lan
---
kind: Network
name: guest-lan
---
kind: Instance
name: node0
---
kind: Instance
name: node1
`
	cfg, err := Parse(strings.NewReader(fleet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Networks) != 2 {
		t.Errorf("len(Networks) = %d, want 2", len(cfg.Networks))
	}
	if len(cfg.Instances) != 2 {
		t.Errorf("len(Instances) = %d, want 2", len(cfg.Instances))
	}
}

func TestParseEmptyInput(t *testing.T) {
	cfg, err := Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Networks) != 0 || len(cfg.Instances) != 0 {
		t.Errorf("expected empty Config, got %+v", cfg)
	}
}

func TestParseMissingKind(t *testing.T) {
	const fleet = `
name: home-lan
cidr: 192.168.1.0/24
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for missing kind, got nil")
	}
}

func TestParseUnrecognizedKind(t *testing.T) {
	const fleet = `
kind: Bogus
name: whatever
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for unrecognized kind, got nil")
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	// A misspelled field must be a hard error, not a silently dropped value:
	// "statc_ip" instead of "static_ip" would otherwise leave the node on
	// DHCP with no warning.
	const fleet = `
kind: Instance
name: node0
network: home-lan
statc_ip: 192.168.1.201
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for unknown field, got nil")
	}
}

func TestParseRejectsUnknownNestedField(t *testing.T) {
	// Strictness must reach into nested mappings too (security.*).
	const fleet = `
kind: Instance
name: node0
security:
  tpm: false
  secur_boot: true
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for unknown nested field, got nil")
	}
}

func TestParseMalformedYAML(t *testing.T) {
	const fleet = `
kind: Network
name: [unterminated
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for malformed YAML, got nil")
	}
}

// The agent is declared as an ordinary App — `type: agent` is not reserved,
// and nothing about it is special-cased in the schema. This is the exact YAML
// from docs/AppManager.md § replicas: per-node; an earlier revision of the
// design gave the agent its own `kind: AgentConfig`, and this pins that the
// replacement really does parse as a plain document.
func TestParseAgentApp(t *testing.T) {
	const fleet = `
kind: App
name: agent
type: agent
replicas: per-node
image:
  server: https://ghcr.io
  protocol: oci
  alias: ehharvey/homelab-ops/agent:latest
`
	cfg, err := Parse(strings.NewReader(fleet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := App{
		Name:     "agent",
		Type:     "agent",
		Replicas: Replicas{PerNode: true},
		Image: ImageRef{
			Server:   "https://ghcr.io",
			Protocol: "oci",
			Alias:    "ehharvey/homelab-ops/agent:latest",
		},
	}
	if len(cfg.Apps) != 1 {
		t.Fatalf("len(Apps) = %d, want 1", len(cfg.Apps))
	}
	if got := cfg.Apps[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("Apps[0] = %+v, want %+v", got, want)
	}
}

func TestParseAppWithCountAndParams(t *testing.T) {
	const fleet = `
kind: App
name: web-frontend
type: some-renderer
replicas: 3
image:
  fingerprint: abc123
params:
  LOG_LEVEL: debug
  EXTRA: "1"
`
	cfg, err := Parse(strings.NewReader(fleet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := App{
		Name:     "web-frontend",
		Type:     "some-renderer",
		Replicas: Replicas{Count: 3},
		Image:    ImageRef{Fingerprint: "abc123"},
		Params:   map[string]string{"LOG_LEVEL": "debug", "EXTRA": "1"},
	}
	if got := cfg.Apps[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("Apps[0] = %+v, want %+v", got, want)
	}
}

// params is opaque renderer passthrough, so strict decoding must NOT reach
// inside it — arbitrary keys are the point. Contrast TestParseRejectsUnknownAppField.
func TestParseAcceptsOpaqueParams(t *testing.T) {
	const fleet = `
kind: App
name: web-frontend
type: some-renderer
replicas: 1
image:
  alias: some/image:latest
params:
  anything_at_all: yes-really
`
	if _, err := Parse(strings.NewReader(fleet)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParseRejectsUnknownAppField(t *testing.T) {
	const fleet = `
kind: App
name: web-frontend
type: some-renderer
replicas: 1
imagee:
  alias: some/image:latest
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for unknown field, got nil")
	}
}

// Strictness must reach into the nested ImageRef mapping too: a typo'd
// `aliass` would otherwise leave the image unresolvable at reconcile time
// rather than failing loudly here.
func TestParseRejectsUnknownImageField(t *testing.T) {
	const fleet = `
kind: App
name: web-frontend
type: some-renderer
replicas: 1
image:
  aliass: some/image:latest
`
	if _, err := Parse(strings.NewReader(fleet)); err == nil {
		t.Fatalf("expected error for unknown nested field, got nil")
	}
}

// yaml.v3 has no notion of a required key: an omitted `replicas:` never calls
// Replicas.UnmarshalText, so it reaches Validate as a zero value rather than a
// parse error. That's precisely why the required-ness rule lives in Validate
// (see TestValidateAppIssues), and this pins the premise it rests on.
func TestParseAppWithoutReplicasIsStructurallyValid(t *testing.T) {
	const fleet = `
kind: App
name: web-frontend
type: some-renderer
image:
  alias: some/image:latest
`
	cfg, err := Parse(strings.NewReader(fleet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := cfg.Apps[0].Replicas, (Replicas{}); got != want {
		t.Errorf("Replicas = %+v, want the zero value %+v", got, want)
	}
}

func TestReplicasUnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    Replicas
		wantErr bool
	}{
		{"empty is the zero value", "", Replicas{}, false},
		{"per-node", "per-node", Replicas{PerNode: true}, false},
		{"count", "3", Replicas{Count: 3}, false},
		{"surrounding space is trimmed", "  per-node  ", Replicas{PerNode: true}, false},
		// Zero and negative counts parse: whether a count is meaningful is a
		// semantic question, and Validate answers it (mirroring Range, which
		// parses a start-after-end range and lets Validate reject it).
		{"zero parses, Validate rejects it", "0", Replicas{Count: 0}, false},
		{"negative parses, Validate rejects it", "-1", Replicas{Count: -1}, false},
		{"unparseable", "abc", Replicas{}, true},
		{"per node without the hyphen", "per node", Replicas{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Replicas
			err := got.UnmarshalText([]byte(tc.text))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalText(%q) = %+v, want an error", tc.text, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalText(%q): %v", tc.text, err)
			}
			if got != tc.want {
				t.Errorf("UnmarshalText(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

// The store round-trips Replicas through a TEXT column via MarshalText, so
// every value — including the zero one — must survive it unchanged.
func TestReplicasMarshalTextRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		r    Replicas
		want string
	}{
		{"zero", Replicas{}, ""},
		{"per-node", Replicas{PerNode: true}, "per-node"},
		{"count", Replicas{Count: 3}, "3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := tc.r.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			if string(b) != tc.want {
				t.Fatalf("MarshalText() = %q, want %q", b, tc.want)
			}
			var got Replicas
			if err := got.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", b, err)
			}
			if got != tc.r {
				t.Errorf("round-trip = %+v, want %+v", got, tc.r)
			}
		})
	}
}
