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
}

// Config holds every Network and Instance document parsed from a fleet
// definition file.
type Config struct {
	Networks  []Network
	Instances []Instance
}

type discriminator struct {
	Kind string `yaml:"kind"`
}

// Parse reads a multi-document, k8s-style YAML fleet definition and returns
// the parsed Networks and Instances. Each document must set `kind: Network`
// or `kind: Instance`; any other or missing kind is an error.
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
