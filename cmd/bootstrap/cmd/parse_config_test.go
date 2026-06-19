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

func TestParseConfigCommandMissingFile(t *testing.T) {
	rootCmd.SetArgs([]string{"parse-config", "--file", "/nonexistent/fleet.yaml"})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err == nil {
		t.Fatalf("expected error for nonexistent file, got nil")
	}
}
