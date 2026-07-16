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
	"os"
	"path/filepath"

	lxcapi "github.com/lxc/incus/v7/shared/api"
	"gopkg.in/yaml.v3"

	incusapi "github.com/ehharvey/homelab-ops/internal/third_party/incusos/api"
	incusseed "github.com/ehharvey/homelab-ops/internal/third_party/incusos/api/seed"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// Options configures behavior that has no equivalent in the fleet
// definition format (config.Instance/config.Network) but is still a knob
// on IncusOS's install seed.
type Options struct {
	ForceInstall bool
	ForceReboot  bool
}

// wireGuardPersistentKeepaliveSeconds keeps a node's NAT mapping to the web
// app alive (docs/Roadmap.md #91's NAT-survival requirement) — set on the
// node's peer entry for the web app, since the node is the side expected to
// sit behind NAT, not the other way around.
const wireGuardPersistentKeepaliveSeconds = 25

// WireGuard carries every input Render needs to embed WireGuard tunnel
// config into an instance's seed: the web app's own live identity/endpoint
// (constant across a deployment) and this instance's persisted per-node
// credential (unique per instance, from nodeprovision.EnsureCredential).
// Passing a nil *WireGuard to Render means this instance's seed carries no
// WireGuard config at all — e.g. the bootstrap CLI's render-seed, used for
// node #0 before any web app (and therefore any tunnel) exists yet.
type WireGuard struct {
	AppPublicKey   wireguard.PublicKey
	AppEndpoint    string // host:port a node dials to reach the web app
	NodePrivateKey wireguard.PrivateKey
	// BootstrapCertPEM is preseeded as a second trusted Incus client cert,
	// alongside clientCertPEM, so the web app can authenticate over the
	// tunnel with a credential whose private key it actually holds (unlike
	// the break-glass one — see docs/Decisions.md §4).
	BootstrapCertPEM []byte
}

// Bundle holds the four rendered seed documents.
type Bundle struct {
	Install      incusseed.Install
	Network      incusseed.Network
	Applications incusseed.Applications
	Incus        incusseed.Incus
}

// supportedApplications is the 0.x fixed application list: incus only, no
// operations-center, per Architecture.md.
var supportedApplications = map[string]bool{"incus": true}

// Render builds a seed Bundle for inst, which must belong to net. Disk and
// NIC must be "single" — multi-disk/multi-NIC instances are out of scope
// for 0.x and rendering one would silently produce a seed that doesn't match
// the operator's intent, so it's an error instead. clientCertPEM is the
// PEM-encoded bootstrap client certificate (gen-cert's client.crt); it is
// preseeded into Incus's own trust store so the node trusts it on first
// boot — without this, "the bootstrap cert authenticates against it" can
// never become true. wg is optional (see WireGuard's doc) — nil renders a
// seed with no WireGuard config at all.
func Render(net config.Network, inst config.Instance, clientCertPEM []byte, wg *WireGuard, opts Options) (Bundle, error) {
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
			return Bundle{}, fmt.Errorf("instance %q: application %q not supported in 0.x", inst.Name, app)
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
		// Name is required by IncusOS (omitting it leaves the interface
		// unconfigured at boot — confirmed by booting a real seeded image).
		// 0.x only supports a single NIC, so one fixed name is enough.
		Name:  "eth0",
		Roles: []string{incusapi.SystemNetworkInterfaceRoleManagement},
	}
	// Addressing semantics (static_ip ∈ CIDR ∈ range, range ∈ CIDR) are
	// validated upstream by config.Validate — both the server sync path and
	// the render-seed CLI run it before Render. Here we only read the typed,
	// already-valid fields to render the seed.
	if inst.StaticIP.IsValid() {
		iface.Addresses = []string{fmt.Sprintf("%s/%d", inst.StaticIP, net.CIDR.Bits())}

		// A static address with no default route leaves the node unable to
		// reach anything outside its own subnet — confirmed by booting a
		// real seeded image (IncusOS's update/Secure-Boot-key checks failed
		// with "network is unreachable" until a route was added).
		if !net.Gateway.IsValid() {
			return Bundle{}, fmt.Errorf("network %q: gateway is required when an instance has a static_ip", net.Name)
		}
		iface.Routes = []incusapi.SystemNetworkRoute{{To: "0.0.0.0/0", Via: net.Gateway.String()}}
	}

	netConfig := incusapi.SystemNetworkConfig{
		Interfaces: []incusapi.SystemNetworkInterface{iface},
	}
	if len(net.DNS) > 0 {
		nameservers := make([]string, len(net.DNS))
		for i, d := range net.DNS {
			nameservers[i] = d.String()
		}
		netConfig.DNS = &incusapi.SystemNetworkDNS{Nameservers: nameservers}
	}

	var bootstrapCertPEM []byte
	if wg != nil {
		if !inst.TunnelIP.IsValid() {
			return Bundle{}, fmt.Errorf("instance %q: wireguard requested but no tunnel_ip assigned", inst.Name)
		}
		netConfig.Wireguard = []incusapi.SystemNetworkWireguard{{
			Name:       "wg0",
			Addresses:  []string{fmt.Sprintf("%s/32", inst.TunnelIP)},
			PrivateKey: wg.NodePrivateKey.Base64(),
			// Tagging wg0 "management" mirrors eth0's role above, on the
			// (unverified against IncusOS's own docs — confirmed only by a
			// real-VM boot, see scripts/validate-issue-91.sh) assumption
			// that this is what makes Incus's API listen on it.
			Roles: []string{incusapi.SystemNetworkInterfaceRoleManagement},
			Peers: []incusapi.SystemNetworkWireguardPeer{{
				PublicKey:           wg.AppPublicKey.Base64(),
				Endpoint:            wg.AppEndpoint,
				AllowedIPs:          []string{fmt.Sprintf("%s/32", wireguard.WebAppAddr)},
				PersistentKeepalive: wireGuardPersistentKeepaliveSeconds,
			}},
		}}
		bootstrapCertPEM = wg.BootstrapCertPEM
	}

	applications := make([]incusseed.Application, 0, len(inst.Applications))
	for _, app := range inst.Applications {
		applications = append(applications, incusseed.Application{Name: app})
	}

	incusPreseed, err := renderIncusPreseed(inst.Name, clientCertPEM, bootstrapCertPEM)
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
// trust certName's client certificate before first boot, plus a second
// bootstrapCertPEM-derived trusted cert when non-nil — the one-time
// credential nodeprovision.CreateInstance authenticates with over the
// WireGuard tunnel, distinct from the standing break-glass cert (see
// WireGuard's doc comment and docs/Decisions.md §4).
func renderIncusPreseed(certName string, clientCertPEM, bootstrapCertPEM []byte) (incusseed.Incus, error) {
	certs := []lxcapi.CertificatesPost{}

	clientEntry, err := certEntry(certName, clientCertPEM)
	if err != nil {
		return incusseed.Incus{}, fmt.Errorf("client certificate: %w", err)
	}
	certs = append(certs, clientEntry)

	if len(bootstrapCertPEM) > 0 {
		bootstrapEntry, err := certEntry(certName+"-bootstrap", bootstrapCertPEM)
		if err != nil {
			return incusseed.Incus{}, fmt.Errorf("bootstrap certificate: %w", err)
		}
		certs = append(certs, bootstrapEntry)
	}

	return incusseed.Incus{
		ApplyDefaults: true,
		Preseed: &lxcapi.InitPreseed{
			InitLocalPreseed: lxcapi.InitLocalPreseed{
				Certificates: certs,
			},
		},
	}, nil
}

// certEntry builds one trusted-client-certificate preseed entry from a PEM
// cert. Incus's CertificatesPost.Certificate field is base64-encoded raw
// X.509 (DER), not PEM, so the PEM wrapper is stripped first.
func certEntry(name string, certPEM []byte) (lxcapi.CertificatesPost, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return lxcapi.CertificatesPost{}, fmt.Errorf("decode certificate: not a PEM-encoded certificate")
	}
	return lxcapi.CertificatesPost{
		CertificatePut: lxcapi.CertificatePut{
			Name:        name,
			Type:        "client",
			Certificate: base64.StdEncoding.EncodeToString(block.Bytes),
		},
	}, nil
}

// Write marshals b's four documents to YAML and writes install.yaml,
// network.yaml, applications.yaml, and incus.yaml into dir. It refuses to
// overwrite existing files unless force is true.
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
