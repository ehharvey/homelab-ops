// Package config parses the bootstrap CLI's k8s-style fleet definition
// format: one or more YAML documents, each discriminated by a `kind:` field,
// into plain Go objects. It performs no semantic validation (CIDR/IP/MAC
// well-formedness) and no IncusOS seed rendering — both are later steps that
// consume the parsed Config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// Network is a parsed `kind: Network` document.
type Network struct {
	Name              string   `yaml:"name"`
	CIDR              string   `yaml:"cidr"`
	Gateway           string   `yaml:"gateway"`
	DHCPExcludedRange string   `yaml:"dhcp_excluded_range"`
	DNS               []string `yaml:"dns"`
}

// Security holds an Instance's TPM/Secure Boot configuration.
type Security struct {
	TPM        bool `yaml:"tpm"`
	SecureBoot bool `yaml:"secure_boot"`
}

// Instance is a parsed `kind: Instance` document.
type Instance struct {
	Name         string   `yaml:"name"`
	MAC          string   `yaml:"mac"`
	Network      string   `yaml:"network"`
	StaticIP     string   `yaml:"static_ip"`
	Disk         string   `yaml:"disk"`
	NIC          string   `yaml:"nic"`
	Security     Security `yaml:"security"`
	Applications []string `yaml:"applications"`
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
