package seed

import (
	"encoding/base64"
	"encoding/pem"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func sampleClientCertPEM(t *testing.T) []byte {
	t.Helper()

	pair, err := cert.Generate(cert.Options{CommonName: "node0", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}

	return pair.CertPEM
}

func sampleNetwork() config.Network {
	return config.Network{
		Name:              "home-lan",
		CIDR:              netip.MustParsePrefix("192.168.1.0/24"),
		Gateway:           netip.MustParseAddr("192.168.1.1"),
		DHCPExcludedRange: config.Range{Start: netip.MustParseAddr("192.168.1.200"), End: netip.MustParseAddr("192.168.1.250")},
		DNS:               []netip.Addr{netip.MustParseAddr("192.168.1.1")},
	}
}

func sampleInstance() config.Instance {
	return config.Instance{
		Name:         "node0",
		MAC:          "aa:bb:cc:dd:ee:ff",
		Network:      "home-lan",
		StaticIP:     netip.MustParseAddr("192.168.1.201"),
		Disk:         "single",
		NIC:          "single",
		Security:     config.Security{TPM: false, SecureBoot: true},
		Applications: []string{"incus"},
	}
}

func TestRenderHappyPath(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !b.Install.Security.MissingTPM {
		t.Errorf("Install.Security.MissingTPM = false, want true (instance has no TPM)")
	}
	if b.Install.Security.MissingSecureBoot {
		t.Errorf("Install.Security.MissingSecureBoot = true, want false (instance has secure boot)")
	}
	if b.Install.Target != nil {
		t.Errorf("Install.Target = %+v, want nil", b.Install.Target)
	}

	if len(b.Network.Interfaces) != 1 {
		t.Fatalf("len(Network.Interfaces) = %d, want 1", len(b.Network.Interfaces))
	}
	iface := b.Network.Interfaces[0]
	if iface.Hwaddr != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("Interfaces[0].Hwaddr = %q, want %q", iface.Hwaddr, "aa:bb:cc:dd:ee:ff")
	}
	if iface.Name == "" {
		t.Errorf("Interfaces[0].Name is empty; IncusOS leaves the interface unconfigured without a name")
	}
	if len(iface.Addresses) != 1 || iface.Addresses[0] != "192.168.1.201/24" {
		t.Errorf("Interfaces[0].Addresses = %v, want [192.168.1.201/24]", iface.Addresses)
	}
	if len(iface.Routes) != 1 || iface.Routes[0].To != "0.0.0.0/0" || iface.Routes[0].Via != "192.168.1.1" {
		t.Errorf("Interfaces[0].Routes = %+v, want default route via 192.168.1.1", iface.Routes)
	}
	if b.Network.DNS == nil || len(b.Network.DNS.Nameservers) != 1 || b.Network.DNS.Nameservers[0] != "192.168.1.1" {
		t.Errorf("Network.DNS = %+v, want Nameservers [192.168.1.1]", b.Network.DNS)
	}

	if len(b.Applications.Applications) != 1 || b.Applications.Applications[0].Name != "incus" {
		t.Errorf("Applications = %+v, want [{incus}]", b.Applications.Applications)
	}
}

func TestRenderDHCP(t *testing.T) {
	inst := sampleInstance()
	inst.StaticIP = netip.Addr{}

	b, err := Render(sampleNetwork(), inst, sampleClientCertPEM(t), nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if len(b.Network.Interfaces[0].Addresses) != 0 {
		t.Errorf("Addresses = %v, want empty (DHCP fallback)", b.Network.Interfaces[0].Addresses)
	}
	if len(b.Network.Interfaces[0].Routes) != 0 {
		t.Errorf("Routes = %+v, want empty (DHCP provides its own default route)", b.Network.Interfaces[0].Routes)
	}
}

func TestRenderRejectsStaticIPWithoutGateway(t *testing.T) {
	net := sampleNetwork()
	net.Gateway = netip.Addr{}

	if _, err := Render(net, sampleInstance(), sampleClientCertPEM(t), nil, Options{}); err == nil {
		t.Fatal("expected error for static_ip without a network gateway, got nil")
	}
}

// Addressing semantics (static_ip ∈ CIDR ∈ range, range ∈ CIDR, well-formed
// range) are no longer Render's responsibility — they're validated upstream by
// config.Validate (see internal/config/validate_test.go). Render only enforces
// the rendering-specific guards below.

func TestRenderRejectsNetworkMismatch(t *testing.T) {
	inst := sampleInstance()
	inst.Network = "other-lan"

	if _, err := Render(sampleNetwork(), inst, sampleClientCertPEM(t), nil, Options{}); err == nil {
		t.Fatalf("expected error for mismatched network, got nil")
	}
}

func TestRenderRejectsUnsupportedDisk(t *testing.T) {
	inst := sampleInstance()
	inst.Disk = "raid1"

	if _, err := Render(sampleNetwork(), inst, sampleClientCertPEM(t), nil, Options{}); err == nil {
		t.Fatalf("expected error for unsupported disk, got nil")
	}
}

func TestRenderRejectsUnsupportedNIC(t *testing.T) {
	inst := sampleInstance()
	inst.NIC = "bond0"

	if _, err := Render(sampleNetwork(), inst, sampleClientCertPEM(t), nil, Options{}); err == nil {
		t.Fatalf("expected error for unsupported nic, got nil")
	}
}

func TestRenderRejectsUnsupportedApplication(t *testing.T) {
	inst := sampleInstance()
	inst.Applications = []string{"incus", "operations-center"}

	if _, err := Render(sampleNetwork(), inst, sampleClientCertPEM(t), nil, Options{}); err == nil {
		t.Fatalf("expected error for unsupported application, got nil")
	}
}

func TestRenderOptionsForwarded(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), nil, Options{ForceInstall: true, ForceReboot: true})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !b.Install.ForceInstall || !b.Install.ForceReboot {
		t.Errorf("Install = %+v, want ForceInstall and ForceReboot true", b.Install)
	}
}

func TestRenderIncusPreseedTrustsClientCert(t *testing.T) {
	certPEM := sampleClientCertPEM(t)

	b, err := Render(sampleNetwork(), sampleInstance(), certPEM, nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !b.Incus.ApplyDefaults {
		t.Errorf("Incus.ApplyDefaults = false, want true")
	}
	if b.Incus.Preseed == nil {
		t.Fatalf("Incus.Preseed = nil, want non-nil")
	}

	certs := b.Incus.Preseed.Certificates
	if len(certs) != 1 {
		t.Fatalf("len(Preseed.Certificates) = %d, want 1", len(certs))
	}
	if certs[0].Name != "node0" {
		t.Errorf("Certificates[0].Name = %q, want %q", certs[0].Name, "node0")
	}
	if certs[0].Type != "client" {
		t.Errorf("Certificates[0].Type = %q, want %q", certs[0].Type, "client")
	}

	gotDER, err := base64.StdEncoding.DecodeString(certs[0].Certificate)
	if err != nil {
		t.Fatalf("Certificates[0].Certificate is not valid base64: %v", err)
	}
	wantBlock, _ := pem.Decode(certPEM)
	if wantBlock == nil {
		t.Fatalf("sampleClientCertPEM did not produce a decodable PEM block")
	}
	if string(gotDER) != string(wantBlock.Bytes) {
		t.Errorf("Certificates[0].Certificate does not match the base64(DER) of the original client cert")
	}
}

func TestRenderWithWireGuard(t *testing.T) {
	inst := sampleInstance()
	inst.TunnelIP = netip.MustParseAddr("10.100.0.5")

	_, appPub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("wireguard.GenerateKeypair: %v", err)
	}
	nodePriv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("wireguard.GenerateKeypair: %v", err)
	}
	bootstrapCertPEM := sampleClientCertPEM(t)

	wg := &WireGuard{
		AppPublicKey:     appPub,
		AppEndpoint:      "203.0.113.1:51820",
		NodePrivateKey:   nodePriv,
		BootstrapCertPEM: bootstrapCertPEM,
	}

	b, err := Render(sampleNetwork(), inst, sampleClientCertPEM(t), wg, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if len(b.Network.Wireguard) != 1 {
		t.Fatalf("len(Network.Wireguard) = %d, want 1", len(b.Network.Wireguard))
	}
	wgIface := b.Network.Wireguard[0]
	if len(wgIface.Addresses) != 1 || wgIface.Addresses[0] != "10.100.0.5/32" {
		t.Errorf("Wireguard[0].Addresses = %v, want [10.100.0.5/32]", wgIface.Addresses)
	}
	if wgIface.PrivateKey != nodePriv.Base64() {
		t.Errorf("Wireguard[0].PrivateKey = %q, want %q", wgIface.PrivateKey, nodePriv.Base64())
	}
	if len(wgIface.Peers) != 1 {
		t.Fatalf("len(Wireguard[0].Peers) = %d, want 1", len(wgIface.Peers))
	}
	peer := wgIface.Peers[0]
	if peer.PublicKey != appPub.Base64() {
		t.Errorf("Peers[0].PublicKey = %q, want %q", peer.PublicKey, appPub.Base64())
	}
	if peer.Endpoint != "203.0.113.1:51820" {
		t.Errorf("Peers[0].Endpoint = %q, want %q", peer.Endpoint, "203.0.113.1:51820")
	}
	if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0] != wireguard.WebAppAddr.String()+"/32" {
		t.Errorf("Peers[0].AllowedIPs = %v, want [%s/32]", peer.AllowedIPs, wireguard.WebAppAddr)
	}
	if peer.PersistentKeepalive != wireGuardPersistentKeepaliveSeconds {
		t.Errorf("Peers[0].PersistentKeepalive = %d, want %d", peer.PersistentKeepalive, wireGuardPersistentKeepaliveSeconds)
	}

	certs := b.Incus.Preseed.Certificates
	if len(certs) != 2 {
		t.Fatalf("len(Preseed.Certificates) = %d, want 2 (break-glass + bootstrap)", len(certs))
	}
	if certs[1].Name != "node0-bootstrap" {
		t.Errorf("Certificates[1].Name = %q, want %q", certs[1].Name, "node0-bootstrap")
	}
}

func TestRenderWithWireGuardRequiresTunnelIP(t *testing.T) {
	_, appPub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("wireguard.GenerateKeypair: %v", err)
	}
	nodePriv, _, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("wireguard.GenerateKeypair: %v", err)
	}
	wg := &WireGuard{AppPublicKey: appPub, AppEndpoint: "203.0.113.1:51820", NodePrivateKey: nodePriv}

	// sampleInstance has no TunnelIP set.
	if _, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), wg, Options{}); err == nil {
		t.Fatal("expected error for wireguard requested without a tunnel_ip, got nil")
	}
}

func TestRenderRejectsInvalidCertPEM(t *testing.T) {
	if _, err := Render(sampleNetwork(), sampleInstance(), []byte("not a cert"), nil, Options{}); err == nil {
		t.Fatal("expected error for invalid client cert PEM, got nil")
	}
}

func TestBundleYAMLMatchesReferenceFieldNames(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	installYAML, err := yaml.Marshal(b.Install)
	if err != nil {
		t.Fatalf("marshal install: %v", err)
	}
	for _, want := range []string{"force_install:", "force_reboot:", "missing_tpm:", "missing_secure_boot:"} {
		if !strings.Contains(string(installYAML), want) {
			t.Errorf("install.yaml missing %q, got:\n%s", want, installYAML)
		}
	}

	networkYAML, err := yaml.Marshal(b.Network)
	if err != nil {
		t.Fatalf("marshal network: %v", err)
	}
	for _, want := range []string{"hwaddr:", "addresses:", "nameservers:", "name: eth0", "routes:", "via: 192.168.1.1"} {
		if !strings.Contains(string(networkYAML), want) {
			t.Errorf("network.yaml missing %q, got:\n%s", want, networkYAML)
		}
	}

	appsYAML, err := yaml.Marshal(b.Applications)
	if err != nil {
		t.Fatalf("marshal applications: %v", err)
	}
	for _, want := range []string{"applications:", "name: incus"} {
		if !strings.Contains(string(appsYAML), want) {
			t.Errorf("applications.yaml missing %q, got:\n%s", want, appsYAML)
		}
	}

	incusYAML, err := yaml.Marshal(b.Incus)
	if err != nil {
		t.Fatalf("marshal incus: %v", err)
	}
	for _, want := range []string{"apply_defaults:", "preseed:", "certificates:", "type: client"} {
		if !strings.Contains(string(incusYAML), want) {
			t.Errorf("incus.yaml missing %q, got:\n%s", want, incusYAML)
		}
	}
}

func TestWriteCreatesAllFourFiles(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dir := t.TempDir()
	if err := Write(dir, b, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, name := range []string{"install.yaml", "network.yaml", "applications.yaml", "incus.yaml"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestWriteRefusesOverwriteWithoutForce(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dir := t.TempDir()
	if err := Write(dir, b, false); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(dir, b, false); err == nil {
		t.Fatalf("expected error on second Write without force, got nil")
	}
}

func TestWriteOverwritesWithForce(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), sampleClientCertPEM(t), nil, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dir := t.TempDir()
	if err := Write(dir, b, false); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(dir, b, true); err != nil {
		t.Fatalf("forced Write: %v", err)
	}
}
