package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/internal/config"
)

func sampleNetwork() config.Network {
	return config.Network{
		Name:              "home-lan",
		CIDR:              "192.168.1.0/24",
		DHCPExcludedRange: "192.168.1.200-192.168.1.250",
		DNS:               []string{"192.168.1.1"},
	}
}

func sampleInstance() config.Instance {
	return config.Instance{
		Name:         "node0",
		MAC:          "aa:bb:cc:dd:ee:ff",
		Network:      "home-lan",
		StaticIP:     "192.168.1.201",
		Disk:         "single",
		NIC:          "single",
		Security:     config.Security{TPM: false, SecureBoot: true},
		Applications: []string{"incus"},
	}
}

func TestRenderHappyPath(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), Options{})
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
	if len(iface.Addresses) != 1 || iface.Addresses[0] != "192.168.1.201/24" {
		t.Errorf("Interfaces[0].Addresses = %v, want [192.168.1.201/24]", iface.Addresses)
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
	inst.StaticIP = ""

	b, err := Render(sampleNetwork(), inst, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if len(b.Network.Interfaces[0].Addresses) != 0 {
		t.Errorf("Addresses = %v, want empty (DHCP fallback)", b.Network.Interfaces[0].Addresses)
	}
}

func TestRenderRejectsNetworkMismatch(t *testing.T) {
	inst := sampleInstance()
	inst.Network = "other-lan"

	if _, err := Render(sampleNetwork(), inst, Options{}); err == nil {
		t.Fatalf("expected error for mismatched network, got nil")
	}
}

func TestRenderRejectsUnsupportedDisk(t *testing.T) {
	inst := sampleInstance()
	inst.Disk = "raid1"

	if _, err := Render(sampleNetwork(), inst, Options{}); err == nil {
		t.Fatalf("expected error for unsupported disk, got nil")
	}
}

func TestRenderRejectsUnsupportedNIC(t *testing.T) {
	inst := sampleInstance()
	inst.NIC = "bond0"

	if _, err := Render(sampleNetwork(), inst, Options{}); err == nil {
		t.Fatalf("expected error for unsupported nic, got nil")
	}
}

func TestRenderRejectsUnsupportedApplication(t *testing.T) {
	inst := sampleInstance()
	inst.Applications = []string{"incus", "operations-center"}

	if _, err := Render(sampleNetwork(), inst, Options{}); err == nil {
		t.Fatalf("expected error for unsupported application, got nil")
	}
}

func TestRenderOptionsForwarded(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), Options{ForceInstall: true, ForceReboot: true})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !b.Install.ForceInstall || !b.Install.ForceReboot {
		t.Errorf("Install = %+v, want ForceInstall and ForceReboot true", b.Install)
	}
}

func TestBundleYAMLMatchesReferenceFieldNames(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), Options{})
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
	for _, want := range []string{"hwaddr:", "addresses:", "nameservers:"} {
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
}

func TestWriteCreatesAllThreeFiles(t *testing.T) {
	b, err := Render(sampleNetwork(), sampleInstance(), Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dir := t.TempDir()
	if err := Write(dir, b, false); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, name := range []string{"install.yaml", "network.yaml", "applications.yaml"} {
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
	b, err := Render(sampleNetwork(), sampleInstance(), Options{})
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
	b, err := Render(sampleNetwork(), sampleInstance(), Options{})
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
