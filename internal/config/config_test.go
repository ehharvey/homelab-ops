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
