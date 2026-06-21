// Package seed renders an IncusOS install seed bundle (install.yaml,
// network.yaml, applications.yaml, incus.yaml) from a parsed
// config.Network/config.Instance pair. The seed types themselves live in
// internal/third_party/incusos (vendored from IncusOS upstream) so the
// rendered YAML matches IncusOS's own reference format by construction.
package seed

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"

	lxcapi "github.com/lxc/incus/v7/shared/api"
	"gopkg.in/yaml.v3"

	incusapi "github.com/ehharvey/homelab-ops/internal/third_party/incusos/api"
	incusseed "github.com/ehharvey/homelab-ops/internal/third_party/incusos/api/seed"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// Options configures behavior that has no equivalent in the fleet
// definition format (config.Instance/config.Network) but is still a knob
// on IncusOS's install seed.
type Options struct {
	ForceInstall bool
	ForceReboot  bool
}

// Bundle holds the four rendered seed documents.
type Bundle struct {
	Install      incusseed.Install
	Network      incusseed.Network
	Applications incusseed.Applications
	Incus        incusseed.Incus
}

// supportedApplications is v1's fixed application list: incus only, no
// operations-center, per Architecture.md.
var supportedApplications = map[string]bool{"incus": true}

// Render builds a seed Bundle for inst, which must belong to net. Disk and
// NIC must be "single" — multi-disk/multi-NIC instances are out of scope
// for v1 and rendering one would silently produce a seed that doesn't match
// the operator's intent, so it's an error instead. clientCertPEM is the
// PEM-encoded bootstrap client certificate (gen-cert's client.crt); it is
// preseeded into Incus's own trust store so the node trusts it on first
// boot — without this, "the bootstrap cert authenticates against it" can
// never become true.
func Render(net config.Network, inst config.Instance, clientCertPEM []byte, opts Options) (Bundle, error) {
	if inst.Network != net.Name {
		return Bundle{}, fmt.Errorf("instance %q targets network %q, not %q", inst.Name, inst.Network, net.Name)
	}
	if inst.Disk != "single" {
		return Bundle{}, fmt.Errorf("instance %q: disk %q not supported (only \"single\" is implemented)", inst.Name, inst.Disk)
	}
	if inst.NIC != "single" {
		return Bundle{}, fmt.Errorf("instance %q: nic %q not supported (only \"single\" is implemented)", inst.Name, inst.NIC)
	}

	for _, app := range inst.Applications {
		if !supportedApplications[app] {
			return Bundle{}, fmt.Errorf("instance %q: application %q not supported in v1", inst.Name, app)
		}
	}

	install := incusseed.Install{
		ForceInstall: opts.ForceInstall,
		ForceReboot:  opts.ForceReboot,
		Security: &incusseed.InstallSecurity{
			MissingTPM:        !inst.Security.TPM,
			MissingSecureBoot: !inst.Security.SecureBoot,
		},
	}

	iface := incusapi.SystemNetworkInterface{
		Hwaddr: inst.MAC,
		Roles:  []string{incusapi.SystemNetworkInterfaceRoleManagement},
	}
	if inst.StaticIP != "" {
		prefix, err := cidrPrefixLen(net.CIDR)
		if err != nil {
			return Bundle{}, fmt.Errorf("network %q: %w", net.Name, err)
		}
		iface.Addresses = []string{fmt.Sprintf("%s/%d", inst.StaticIP, prefix)}
	}

	netConfig := incusapi.SystemNetworkConfig{
		Interfaces: []incusapi.SystemNetworkInterface{iface},
	}
	if len(net.DNS) > 0 {
		netConfig.DNS = &incusapi.SystemNetworkDNS{Nameservers: net.DNS}
	}

	applications := make([]incusseed.Application, 0, len(inst.Applications))
	for _, app := range inst.Applications {
		applications = append(applications, incusseed.Application{Name: app})
	}

	incusPreseed, err := renderIncusPreseed(inst.Name, clientCertPEM)
	if err != nil {
		return Bundle{}, fmt.Errorf("render incus.yaml: %w", err)
	}

	return Bundle{
		Install:      install,
		Network:      incusseed.Network{SystemNetworkConfig: netConfig},
		Applications: incusseed.Applications{Applications: applications},
		Incus:        incusPreseed,
	}, nil
}

// renderIncusPreseed builds the Incus seed file that preconfigures Incus to
// trust certName's client certificate before first boot. Incus's
// CertificatesPost.Certificate field is base64-encoded raw X.509 (DER), not
// PEM, so the PEM wrapper is stripped first.
func renderIncusPreseed(certName string, clientCertPEM []byte) (incusseed.Incus, error) {
	block, _ := pem.Decode(clientCertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return incusseed.Incus{}, fmt.Errorf("decode client certificate: not a PEM-encoded certificate")
	}

	return incusseed.Incus{
		ApplyDefaults: true,
		Preseed: &lxcapi.InitPreseed{
			InitLocalPreseed: lxcapi.InitLocalPreseed{
				Certificates: []lxcapi.CertificatesPost{
					{
						CertificatePut: lxcapi.CertificatePut{
							Name:        certName,
							Type:        "client",
							Certificate: base64.StdEncoding.EncodeToString(block.Bytes),
						},
					},
				},
			},
		},
	}, nil
}

// cidrPrefixLen returns the prefix length (e.g. 24 for a /24) of a CIDR
// string such as "192.168.1.0/24".
func cidrPrefixLen(cidr string) (int, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	size, _ := ipNet.Mask.Size()
	return size, nil
}

// Write marshals b's three documents to YAML and writes install.yaml,
// network.yaml, and applications.yaml into dir. It refuses to overwrite
// existing files unless force is true.
func Write(dir string, b Bundle, force bool) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	files := []struct {
		name string
		doc  any
	}{
		{"install.yaml", b.Install},
		{"network.yaml", b.Network},
		{"applications.yaml", b.Applications},
		{"incus.yaml", b.Incus},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if !force {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
		}

		out, err := yaml.Marshal(f.doc)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", f.name, err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil { //nolint:gosec // seed YAML is not secret, 0644 is intentional
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}

	return nil
}
