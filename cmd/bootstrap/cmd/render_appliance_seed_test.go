package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// resetApplianceSeedFlags puts every render-appliance-seed flag back to its
// default, cobra's Changed state included, once the test is done, so no
// later test in the package inherits this one's values.
func resetApplianceSeedFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		renderApplianceSeedCmd.Flags().VisitAll(func(f *pflag.Flag) {
			if err := f.Value.Set(f.DefValue); err != nil {
				t.Errorf("resetting --%s: %v", f.Name, err)
			}
			f.Changed = false
		})
	})
}

// applianceConfigDir writes a config directory render-appliance-seed takes.
func applianceConfigDir(t *testing.T, dir string) string {
	t.Helper()
	cfg := filepath.Join(dir, "web")
	if err := os.MkdirAll(filepath.Join(cfg, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "web.env"), []byte("WIREGUARD_ENDPOINT=web.example.net:51820\nCONFIG_REPO_REF=main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "secrets", "wireguard.key"), []byte("key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRenderApplianceSeedCommandWritesSeed(t *testing.T) {
	dir := t.TempDir()
	cfg := applianceConfigDir(t, dir)
	// Clutter the seed leaves out, and says so.
	if err := os.WriteFile(filepath.Join(cfg, "web.env~"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(keys, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl op@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "seed")
	digest := "sha256:" + strings.Repeat("ab", 32)

	resetApplianceSeedFlags(t)
	rootCmd.SetArgs([]string{"render-appliance-seed", "--config-dir", cfg, "--output", out,
		"--image", digest, "--reboot-time", "02:30", "--no-secrets", "--ssh-authorized-keys", keys})
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("render-appliance-seed: %v\n%s%s", err, stdout.String(), stderr.String())
	}

	ud, err := os.ReadFile(filepath.Join(out, "user-data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/etc/homelab-ops/web/web.env", "@" + digest, `"rebootTime":"02:30"`} {
		if !bytes.Contains(ud, []byte(want)) {
			t.Errorf("user-data lacks %q", want)
		}
	}
	if bytes.Contains(ud, []byte("wireguard.key")) {
		t.Error("--no-secrets user-data carries secrets/wireguard.key")
	}
	if _, err := os.Stat(filepath.Join(out, "meta-data")); err != nil {
		t.Error(err)
	}
	if !strings.Contains(stdout.String(), "secrets/ left out") {
		t.Errorf("output %q doesn't say secrets/ was left out", stdout.String())
	}
	if !strings.Contains(stdout.String(), "web.env~") || bytes.Contains(ud, []byte("web.env~")) {
		t.Errorf("web.env~: output %q should name it as skipped, and user-data leave it out", stdout.String())
	}
	if strings.Contains(stderr.String(), "no shell") {
		t.Errorf("SSH keys given, but stderr says there is no shell: %q", stderr.String())
	}
}

func TestRenderApplianceSeedCommandNotesNoShellWithoutSSHKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := applianceConfigDir(t, dir)
	out := filepath.Join(dir, "seed")

	resetApplianceSeedFlags(t)
	rootCmd.SetArgs([]string{"render-appliance-seed", "--config-dir", cfg, "--output", out})
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("render-appliance-seed: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "will have no shell") {
		t.Errorf("no --ssh-authorized-keys: stderr %q should say the appliance will have no shell", stderr.String())
	}
	if strings.Contains(stdout.String(), "no shell") {
		t.Errorf("the no-shell note belongs on stderr, not stdout: %q", stdout.String())
	}
}
