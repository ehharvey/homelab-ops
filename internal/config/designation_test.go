package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// designationFleet is a whole repo as the agent reads it: a Designation
// alongside the Instance it names.
const designationFleet = `
kind: Network
name: home-lan
cidr: 192.168.1.0/24
gateway: 192.168.1.1
---
kind: Instance
name: node0
mac: 02:00:00:00:00:01
network: home-lan
static_ip: 192.168.1.201
disk: single
nic: single
applications: [incus]
---
kind: Designation
primary: node0
`

func TestParseDesignation(t *testing.T) {
	cfg, err := Parse(strings.NewReader(designationFleet))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := []Designation{{Primary: "node0"}}; !reflect.DeepEqual(cfg.Designations, want) {
		t.Fatalf("Designations = %+v, want %+v", cfg.Designations, want)
	}
	if got := cfg.Primary(); got != "node0" {
		t.Errorf("Primary() = %q, want node0", got)
	}
	if issues := Validate(cfg); !issues.Empty() {
		t.Errorf("Validate() = %v, want no issues", issues)
	}
}

// The epoch is gone (#212): a repo still carrying one must fail loudly rather
// than have the field silently dropped, so an operator who raises it expecting
// a failover finds out it no longer does anything.
func TestParseDesignationRejectsEpochAndUnknownFields(t *testing.T) {
	for _, field := range []string{"epoch: 2", "primaryy: node1"} {
		t.Run(field, func(t *testing.T) {
			doc := "kind: Designation\nprimary: node0\n" + field + "\n"
			if _, err := Parse(strings.NewReader(doc)); err == nil {
				t.Fatalf("Parse accepted %q", field)
			}
		})
	}
}

// Parse stays cardinality-agnostic, as for every other kind: two
// Designations parse, and Validate is what rejects the second.
func TestParseCollectsEveryDesignation(t *testing.T) {
	cfg, err := Parse(strings.NewReader("kind: Designation\nprimary: a\n---\nkind: Designation\nprimary: b\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Designations) != 2 {
		t.Fatalf("len(Designations) = %d, want 2", len(cfg.Designations))
	}
}

func TestPrimaryWithNoDesignation(t *testing.T) {
	if got := (Config{}).Primary(); got != "" {
		t.Errorf("Primary() = %q, want empty: no designation means no leader", got)
	}
}

func TestValidateDesignation(t *testing.T) {
	net0 := Network{Name: "lan", CIDR: netip.MustParsePrefix("10.0.0.0/24")}
	instances := []Instance{{Name: "node0", Network: "lan"}, {Name: "node1", Network: "lan"}}
	tests := []struct {
		name         string
		designations []Designation
		wantPath     string // "" means no issue
		wantMsg      string
	}{
		{"none is valid: no agent leads", nil, "", ""},
		{"one naming a declared instance", []Designation{{Primary: "node1"}}, "", ""},
		{"empty primary", []Designation{{Primary: ""}}, "designations[0].primary", "must name an Instance"},
		{"blank primary", []Designation{{Primary: "  "}}, "designations[0].primary", "must name an Instance"},
		{"unknown instance", []Designation{{Primary: "node9"}}, "designations[0].primary", `unknown instance "node9"`},
		{"names match exactly, not case-folded", []Designation{{Primary: "Node0"}}, "designations[0].primary", "unknown instance"},
		{"a second designation", []Designation{{Primary: "node0"}, {Primary: "node1"}}, "designations[1]", "at most one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(Config{Networks: []Network{net0}, Instances: instances, Designations: tc.designations})
			if tc.wantPath == "" {
				if !issues.Empty() {
					t.Fatalf("Validate() = %v, want no issues", issues)
				}
				return
			}
			if !issueContains(issues, tc.wantPath, tc.wantMsg) {
				t.Fatalf("Validate() = %v, want an issue at %q containing %q", issues, tc.wantPath, tc.wantMsg)
			}
		})
	}
}

// Append is the one place a repo's files merge, so it must carry every kind —
// a kind it forgot would parse and then vanish.
func TestAppendCarriesEveryKind(t *testing.T) {
	var cfg Config
	cfg.Append(Config{Networks: []Network{{Name: "a"}}, Apps: []App{{Name: "x"}}})
	cfg.Append(Config{Instances: []Instance{{Name: "n"}}, Designations: []Designation{{Primary: "n"}}})
	want := Config{
		Networks:     []Network{{Name: "a"}},
		Instances:    []Instance{{Name: "n"}},
		Apps:         []App{{Name: "x"}},
		Designations: []Designation{{Primary: "n"}},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Append = %+v, want %+v", cfg, want)
	}
	// Guard the test itself: if Config grows a field, this must grow with it.
	if n := reflect.TypeOf(Config{}).NumField(); n != 4 {
		t.Errorf("Config has %d fields; update Append and this test", n)
	}
}
