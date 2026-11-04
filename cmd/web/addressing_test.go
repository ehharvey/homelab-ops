package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/addressing"
	"github.com/ehharvey/homelab-ops/internal/deployconfig"
	"github.com/ehharvey/homelab-ops/internal/server"
)

// TestNetworkAddressing: CONFIG_REPO_PRIVATE becomes RepoPrivate, and
// NETWORK_ADDRESSING_PATH, when set, the addressing file (#199). Their
// parsing and defaults are internal/deployconfig's tests.
func TestNetworkAddressing(t *testing.T) {
	tests := []struct {
		private bool
		path    string
	}{
		{false, ""},
		{true, ""},
		{false, "/cfg/addressing.yaml"},
		{true, "/cfg/addressing.yaml"},
	}
	for _, tc := range tests {
		a := networkAddressing(deployconfig.Config{ConfigRepoPrivate: tc.private, NetworkAddressingPath: tc.path})
		if a.RepoPrivate != tc.private {
			t.Errorf("ConfigRepoPrivate=%v: RepoPrivate = %v", tc.private, a.RepoPrivate)
		}
		switch f, ok := a.State.(addressing.File); {
		case tc.path == "" && a.State != nil:
			t.Errorf("NETWORK_ADDRESSING_PATH unset: State = %v, want nil", a.State)
		case tc.path != "" && (!ok || f.Path != tc.path):
			t.Errorf("NETWORK_ADDRESSING_PATH=%q: State = %#v, want addressing.File{%q}", tc.path, a.State, tc.path)
		}
	}
}

// TestCheckAddressing: a NETWORK_ADDRESSING_PATH that can't be read or
// parsed, or whose entries fail the checks that need no git, stops startup,
// naming the setting; a sound one passes.
func TestCheckAddressing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	good := filepath.Join(dir, "networks.yaml")
	if err := os.WriteFile(good, []byte("kind: Network\nname: lab\ncidr: 10.0.2.0/24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("kind: Instance\nname: node0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Parses, but an entry with no cidr fails addressing.CheckEntries.
	noCIDR := filepath.Join(dir, "nocidr.yaml")
	if err := os.WriteFile(noCIDR, []byte("kind: Network\nname: lab\ngateway: 10.0.2.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path    string
		wantErr bool
	}{
		{good, false},
		{filepath.Join(dir, "typo.yaml"), true},
		{bad, true},
		{noCIDR, true},
	} {
		err := checkAddressing(ctx, server.Addressing{State: addressing.File{Path: tc.path}})
		if (err != nil) != tc.wantErr || (err != nil && !strings.Contains(err.Error(), "NETWORK_ADDRESSING_PATH")) {
			t.Errorf("checkAddressing(%s) = %v, want error %v naming NETWORK_ADDRESSING_PATH", filepath.Base(tc.path), err, tc.wantErr)
		}
	}
	if err := checkAddressing(ctx, server.Addressing{}); err != nil {
		t.Errorf("checkAddressing with no source = %v, want only a warning", err)
	}
}
