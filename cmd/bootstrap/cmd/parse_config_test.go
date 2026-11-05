package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

const parseConfigFixture = `
kind: Network
name: home-lan
cidr: 192.168.1.0/24
---
kind: Instance
name: node0
mac: aa:bb:cc:dd:ee:ff
network: home-lan
`

func TestParseConfigCommandSummarizesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte(parseConfigFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	rootCmd.SetArgs([]string{"parse-config", "--file", path})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got := out.String()
	if !bytes.Contains([]byte(got), []byte("home-lan")) {
		t.Errorf("output missing network name, got: %s", got)
	}
	if !bytes.Contains([]byte(got), []byte("node0")) {
		t.Errorf("output missing instance name, got: %s", got)
	}
}

// TestParseConfigCommandReportsDHCPRanges: each network's mode and its
// effective static range and pool, marked written or derived, with the
// uncarved range when carving changed it.
func TestParseConfigCommandReportsDHCPRanges(t *testing.T) {
	const fleet = `
kind: Network
name: proxy-lan
cidr: 10.1.0.0/24
gateway: 10.1.0.1
dhcp:
  mode: proxy
  pool: 10.1.0.100-10.1.0.199
---
kind: Network
name: full-lan
cidr: 10.0.0.0/24
gateway: 10.0.0.1
dns: [10.0.0.53, 1.1.1.1]
dhcp:
  mode: full
---
kind: Network
name: no-ranges
cidr: 10.3.0.0/24
---
kind: Network
name: names-only
---
kind: Network
name: partial
gateway: 10.2.0.1
dns: [10.2.0.53]
`
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := os.WriteFile(path, []byte(fleet), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	rootCmd.SetArgs([]string{"parse-config", "--file", path})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"Network: proxy-lan (cidr=10.1.0.0/24, dhcp.mode=proxy)",
		"static range: 10.1.0.2-10.1.0.99, 10.1.0.200-10.1.0.254 (derived: the cidr minus dhcp.pool; carved from 10.1.0.0-10.1.0.99, 10.1.0.200-10.1.0.255)",
		"pool:         10.1.0.100-10.1.0.199 (written as dhcp.pool)\n",
		"Network: full-lan (cidr=10.0.0.0/24, dhcp.mode=full)",
		"static range: none",
		"pool:         10.0.0.2-10.0.0.52, 10.0.0.54-10.0.0.254 (derived: the whole cidr; carved from 10.0.0.0-10.0.0.255)",
		"node addresses from the pool",
		"Network: proxy-lan (cidr=10.1.0.0/24, dhcp.mode=proxy)\n    static range: 10.1.0.2-10.1.0.99, 10.1.0.200-10.1.0.254 (derived: the cidr minus dhcp.pool; carved from 10.1.0.0-10.1.0.99, 10.1.0.200-10.1.0.255)\n    pool:         10.1.0.100-10.1.0.199 (written as dhcp.pool)\n    node addresses from the static range\n",
		"Network: no-ranges (cidr=10.3.0.0/24, dhcp.mode=off)\n    static range: none (neither dhcp_excluded_range nor dhcp.pool is written)\n    pool:         none (neither dhcp_excluded_range nor dhcp.pool is written)\n    node addresses: no auto-assignment range, so each instance needs a static_ip, anywhere in the cidr\n",
		"Network: names-only (cidr=none, dhcp.mode=off)\n    no addressing: in a fleet the web app syncs, it comes from the addressing file\n",
		"Network: partial (cidr=none, dhcp.mode=off)\n    partial addressing (gateway, dns, no cidr): set its cidr, or move all of it to the addressing file\n",
	} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("output lacks %q, got:\n%s", want, got)
		}
	}
}

func TestParseConfigCommandMissingFile(t *testing.T) {
	rootCmd.SetArgs([]string{"parse-config", "--file", "/nonexistent/fleet.yaml"})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err == nil {
		t.Fatalf("expected error for nonexistent file, got nil")
	}
}
