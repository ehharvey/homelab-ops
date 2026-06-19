package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const renderSeedFixture = `
kind: Network
name: home-lan
cidr: 192.168.1.0/24
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

func TestRenderSeedCommandWritesThreeFiles(t *testing.T) {
	dir := t.TempDir()
	fleetPath := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(fleetPath, []byte(renderSeedFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	outDir := filepath.Join(dir, "out")

	rootCmd.SetArgs([]string{"render-seed", "--file", fleetPath, "--output-dir", outDir})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	for _, name := range []string{"install.yaml", "network.yaml", "applications.yaml"} {
		path := filepath.Join(outDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist: %v", path, err)
		}
	}
}

func TestRenderSeedCommandMissingFile(t *testing.T) {
	rootCmd.SetArgs([]string{"render-seed", "--file", "/nonexistent/fleet.yaml", "--output-dir", t.TempDir()})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err == nil {
		t.Fatalf("expected error for nonexistent file, got nil")
	}
}

func TestRenderSeedCommandRequiresExactlyOneNetworkAndInstance(t *testing.T) {
	dir := t.TempDir()
	fleetPath := filepath.Join(dir, "fleet.yaml")
	const fleet = `
kind: Network
name: home-lan
---
kind: Network
name: guest-lan
---
kind: Instance
name: node0
network: home-lan
disk: single
nic: single
`
	if err := os.WriteFile(fleetPath, []byte(fleet), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	rootCmd.SetArgs([]string{"render-seed", "--file", fleetPath, "--output-dir", t.TempDir()})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err == nil {
		t.Fatalf("expected error for multiple Networks, got nil")
	}
}
