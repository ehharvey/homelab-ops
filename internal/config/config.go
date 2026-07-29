// Package config parses the bootstrap CLI's k8s-style fleet definition
// format: one or more YAML documents, each discriminated by a `kind:` field,
// into plain Go objects. Parse performs structural validation only (strict
// unknown-field detection); address fields are typed with net/netip, so
// their *syntactic* well-formedness is checked at parse time via
// encoding.TextUnmarshaler (which yaml.v3 honors — see
// ExampleNetwork_yamlUnmarshal). Cross-field *semantic* validation
// (gateway-in-CIDR, range-in-CIDR, etc.) is a separate pass, Validate.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Network is a parsed `kind: Network` document. The address fields are typed
// with net/netip rather than strings: yaml.v3 honors encoding.TextUnmarshaler,
// so each parses (and is syntactically validated) straight from the YAML, and
// an empty (`gateway: ""`) or omitted optional lands as a zero value with
// IsValid()==false and no error. See ExampleNetwork_yamlUnmarshal.
type Network struct {
	Name              string       `yaml:"name"`
	CIDR              netip.Prefix `yaml:"cidr"`
	Gateway           netip.Addr   `yaml:"gateway"`
	DHCPExcludedRange Range        `yaml:"dhcp_excluded_range"`
	DNS               []netip.Addr `yaml:"dns"`
}

// Range is a closed inclusive address range written as "<start>-<end>" (used
// for a Network's DHCP-excluded range). It implements encoding.TextMarshaler/
// TextUnmarshaler so it round-trips through YAML, JSON, and the store's TEXT
// columns. The zero Range (unset/omitted) has an invalid Start; detect it with
// !r.Start.IsValid().
type Range struct {
	Start netip.Addr
	End   netip.Addr
}

// UnmarshalText parses "<start>-<end>" into the Range. An empty input yields
// the zero Range with no error, so an omitted/blank dhcp_excluded_range is a
// valid "unset" rather than a parse failure.
func (r *Range) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*r = Range{}
		return nil
	}
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return fmt.Errorf("range %q: want \"<start>-<end>\"", s)
	}
	a, err := netip.ParseAddr(strings.TrimSpace(start))
	if err != nil {
		return fmt.Errorf("range %q: start: %w", s, err)
	}
	b, err := netip.ParseAddr(strings.TrimSpace(end))
	if err != nil {
		return fmt.Errorf("range %q: end: %w", s, err)
	}
	*r = Range{Start: a, End: b}
	return nil
}

// MarshalText renders the Range as "<start>-<end>", or the empty string for
// the zero Range (mirroring netip.Addr's empty-string encoding of its zero
// value, so the store's TEXT round-trip is lossless).
func (r Range) MarshalText() ([]byte, error) {
	if !r.Start.IsValid() {
		return []byte{}, nil
	}
	return []byte(r.Start.String() + "-" + r.End.String()), nil
}

// Security holds an Instance's TPM/Secure Boot configuration.
type Security struct {
	TPM        bool `yaml:"tpm"`
	SecureBoot bool `yaml:"secure_boot"`
}

// Instance is a parsed `kind: Instance` document. StaticIP is a netip.Addr
// (zero/invalid when unset → DHCP); MAC stays a string because
// net.HardwareAddr has no stdlib TextUnmarshaler.
type Instance struct {
	Name         string     `yaml:"name"`
	MAC          string     `yaml:"mac"`
	Network      string     `yaml:"network"`
	StaticIP     netip.Addr `yaml:"static_ip"`
	Disk         string     `yaml:"disk"`
	NIC          string     `yaml:"nic"`
	Security     Security   `yaml:"security"`
	Applications []string   `yaml:"applications"`
	// TunnelIP is this instance's assigned address on the WireGuard overlay
	// (internal/wireguard.OverlayCIDR) — always app-assigned
	// (internal/wireguard.AssignTunnelIPs), never accepted from synced
	// fleet YAML, unlike StaticIP.
	TunnelIP netip.Addr `yaml:"-"`
}

// Replicas is an App's cardinality: a fixed count, or one per known Instance
// ("per-node" — the DaemonSet shape, used by the agent itself and by Alloy).
// It implements encoding.TextUnmarshaler, exactly as Range does for a
// Network's dhcp_excluded_range, so "3" and "per-node" both parse and are
// syntactically validated at parse time (yaml.v3 honors TextUnmarshaler — see
// ExampleNetwork_yamlUnmarshal). The field is required: the zero Replicas is
// not a default meaning 1, it's an unset field Validate rejects.
//
// This is cardinality, NOT placement: it says how many, never where. A
// placement field (targeting an Incus cluster group) is anticipated as a
// separate field, invalid in combination with PerNode — see
// docs/Out of Scope.md. App.Node was deleted precisely because it meant both
// at once; Replicas must not inherit that conflation.
type Replicas struct {
	PerNode bool
	Count   int
}

// perNode is the one non-numeric replicas value.
const perNode = "per-node"

// UnmarshalText parses "per-node" or a decimal count. An empty input yields
// the zero Replicas with no error, mirroring Range: whether an unset field is
// acceptable is Validate's call, not the parser's. A count that is zero or
// negative parses here and is rejected by Validate too, exactly as Range
// parses a start-after-end range and leaves that semantics to Validate.
func (r *Replicas) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*r = Replicas{}
		return nil
	}
	if s == perNode {
		*r = Replicas{PerNode: true}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("replicas %q: want %q or a count", s, perNode)
	}
	*r = Replicas{Count: n}
	return nil
}

// MarshalText renders "per-node" or the count, and the empty string for the
// zero Replicas (mirroring Range's empty-string encoding of its zero value, so
// the store's TEXT round-trip is lossless).
func (r Replicas) MarshalText() ([]byte, error) {
	switch {
	case r.PerNode:
		return []byte(perNode), nil
	case r.Count == 0:
		return []byte{}, nil
	default:
		return []byte(strconv.Itoa(r.Count)), nil
	}
}

// ImageRef mirrors lxcapi.InstanceSource's remote-image fields closely enough
// to build one directly. Confirmed real-world shape: Protocol "oci" with
// Server "https://ghcr.io" pulls straight from GHCR (Incus's OCI remote
// support, shared/cliconfig/remote.go); the local dev/validation loop instead
// points Server at a docker-compose "registry" service.
type ImageRef struct {
	Server      string `yaml:"server,omitempty"`
	Protocol    string `yaml:"protocol,omitempty"` // "oci" | "simplestreams" | "incus" | ""
	Alias       string `yaml:"alias,omitempty"`
	Fingerprint string `yaml:"fingerprint,omitempty"`
}

// App is a parsed `kind: App` document: a workload instance the app-manager
// agent fleet reconciles against live Incus, via the renderer registered for
// Type. Distinct from Instance.Applications (IncusOS host-level applications,
// e.g. "incus" itself) — an App is a workload the agent manages, not a
// host-level IncusOS application. No placement field: Incus's own scheduler
// decides where an App runs in 0.x.
type App struct {
	Name     string            `yaml:"name"`     // unique across the fleet
	Type     string            `yaml:"type"`     // renderer-registry key
	Replicas Replicas          `yaml:"replicas"` // how many; required
	Image    ImageRef          `yaml:"image"`
	Params   map[string]string `yaml:"params,omitempty"` // opaque, renderer-specific passthrough
}

// Config holds every Network, Instance, and App document parsed from a fleet
// definition file.
type Config struct {
	Networks  []Network
	Instances []Instance
	Apps      []App
}

type discriminator struct {
	Kind string `yaml:"kind"`
}

// Parse reads a multi-document, k8s-style YAML fleet definition and returns
// the parsed Networks, Instances, and Apps. Each document must set
// `kind: Network`, `kind: Instance`, or `kind: App`; any other or missing kind
// is an error.
func Parse(r io.Reader) (Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(r)
	for i := 0; ; i++ {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				return cfg, nil
			}
			return Config{}, fmt.Errorf("decode document %d: %w", i, err)
		}

		var d discriminator
		if err := node.Decode(&d); err != nil {
			return Config{}, fmt.Errorf("decode document %d: %w", i, err)
		}

		switch d.Kind {
		case "Network":
			// Inline the discriminator alongside Network so the required
			// kind field isn't itself flagged as unknown by the strict decode.
			var doc struct {
				Kind    string `yaml:"kind"`
				Network `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as Network: %w", i, err)
			}
			cfg.Networks = append(cfg.Networks, doc.Network)
		case "Instance":
			var doc struct {
				Kind     string `yaml:"kind"`
				Instance `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as Instance: %w", i, err)
			}
			cfg.Instances = append(cfg.Instances, doc.Instance)
		case "App":
			var doc struct {
				Kind string `yaml:"kind"`
				App  `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as App: %w", i, err)
			}
			cfg.Apps = append(cfg.Apps, doc.App)
		case "":
			return Config{}, fmt.Errorf("document %d: missing required field %q", i, "kind")
		default:
			return Config{}, fmt.Errorf("document %d: unrecognized kind %q", i, d.Kind)
		}
	}
}

// strictDecode re-decodes an already-parsed document node into out with
// unknown-field detection on, so a typo'd or misplaced key (e.g. "statc_ip"
// silently becoming a DHCP fallback) is a parse error rather than a silently
// dropped field — these configs provision real hardware. yaml.Node.Decode
// can't do this itself: KnownFields is a yaml.Decoder option the node-decode
// path bypasses, so the node is round-tripped back through a strict Decoder.
func strictDecode(node *yaml.Node, out any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("re-encode document: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}
