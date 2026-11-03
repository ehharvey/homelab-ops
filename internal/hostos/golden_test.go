package hostos

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/seed"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// update rewrites the golden files from seed.Render's own output:
//
//	go test ./internal/hostos -run TestIncusOSSeedGolden -update
//
// Don't run it to make a failing case pass: these files were captured from
// seed.Render before the hostos seam existed (#398), and a diff here means
// the seed a node boots with changed.
var update = flag.Bool("update", false, "rewrite testdata/incusos golden files from seed.Render")

// seedFileNames are IncusOS's four seed files, in the order seed.Marshal
// returns them.
var seedFileNames = []string{"install.yaml", "network.yaml", "applications.yaml", "incus.yaml"}

// goldenCase is one fixed seed-render input. Every input is fixed, the certs
// and WireGuard keys included, so the rendered bytes are too.
type goldenCase struct {
	name string
	net  config.Network
	inst config.Instance
	wg   bool
	opts seed.Options
}

func goldenNetwork() config.Network {
	return config.Network{
		Name:              "home-lan",
		CIDR:              netip.MustParsePrefix("192.168.1.0/24"),
		Gateway:           netip.MustParseAddr("192.168.1.1"),
		DHCPExcludedRange: config.Range{Start: netip.MustParseAddr("192.168.1.200"), End: netip.MustParseAddr("192.168.1.250")},
		DNS:               []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("1.1.1.1")},
	}
}

func goldenInstance() config.Instance {
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

func goldenCases() []goldenCase {
	dhcp := goldenInstance()
	dhcp.StaticIP = netip.Addr{}

	// os written out explicitly must render exactly as an omitted one.
	tunneled := goldenInstance()
	tunneled.OS = config.OSIncusOS
	tunneled.TunnelIP = netip.MustParseAddr("10.100.0.2")

	return []goldenCase{
		// render-seed's shape: static address, no tunnel.
		{name: "static", net: goldenNetwork(), inst: goldenInstance()},
		{name: "dhcp", net: goldenNetwork(), inst: dhcp, opts: seed.Options{ForceInstall: true}},
		// The web app's shape: static address plus the WireGuard tunnel and
		// the one-time bootstrap cert.
		{name: "wireguard", net: goldenNetwork(), inst: tunneled, wg: true, opts: seed.Options{ForceReboot: true}},
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "incusos", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func fixedKey(seedByte byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = seedByte + byte(i)
	}
	return k
}

func (c goldenCase) seedWireGuard(t *testing.T) *seed.WireGuard {
	if !c.wg {
		return nil
	}
	return &seed.WireGuard{
		AppPublicKey:     wireguard.PublicKey(fixedKey(0x10)),
		AppEndpoint:      "203.0.113.7:51820",
		NodePrivateKey:   wireguard.PrivateKey(fixedKey(0x80)),
		BootstrapCertPEM: readFixture(t, "bootstrap.crt"),
	}
}

// renderViaSeed is internal/seed alone, without the seam: seed.Render, then
// seed.Marshal. (Before #398, render-seed and the web app's image route wrote
// these bytes with seed.Write, which hostos.WriteFiles replaced; the golden
// files were captured from that seed.Write output.)
func renderViaSeed(t *testing.T, c goldenCase) map[string][]byte {
	t.Helper()
	b, err := seed.Render(c.net, c.inst, readFixture(t, "client.crt"), c.seedWireGuard(t), c.opts)
	if err != nil {
		t.Fatalf("seed.Render: %v", err)
	}
	files, err := seed.Marshal(b)
	if err != nil {
		t.Fatalf("seed.Marshal: %v", err)
	}
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		out[f.Name] = f.Data
	}
	return out
}

// renderViaHostOS is the seam's path, as render-seed and the web app take it
// now: hostos.RenderConfig, then WriteFiles.
func renderViaHostOS(t *testing.T, c goldenCase) map[string][]byte {
	t.Helper()
	in := ConfigInput{
		Network:       c.net,
		Instance:      c.inst,
		ClientCertPEM: readFixture(t, "client.crt"),
		ForceInstall:  c.opts.ForceInstall,
		ForceReboot:   c.opts.ForceReboot,
	}
	if wg := c.seedWireGuard(t); wg != nil {
		in.Tunnel = &Tunnel{
			AppPublicKey:     wg.AppPublicKey,
			AppEndpoint:      wg.AppEndpoint,
			NodePrivateKey:   wg.NodePrivateKey,
			BootstrapCertPEM: wg.BootstrapCertPEM,
		}
	}
	files, err := RenderConfig(in)
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	for i, f := range files {
		if i >= len(seedFileNames) || f.Name != seedFileNames[i] {
			t.Fatalf("RenderConfig file %d is %q, want the order %v", i, f.Name, seedFileNames)
		}
	}
	if len(files) != len(seedFileNames) {
		t.Fatalf("RenderConfig returned %d files, want %d", len(files), len(seedFileNames))
	}
	dir := t.TempDir()
	if err := WriteFiles(dir, files, false); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	return readDir(t, dir)
}

// readDir reads the seed files in dir, failing on any other file there.
func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range seedFileNames {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // test temp dir
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = b
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != len(seedFileNames) {
		t.Fatalf("%s holds %d files, want %d", dir, len(entries), len(seedFileNames))
	}
	return out
}

// TestIncusOSSeedGolden pins the IncusOS seed bundle byte for byte, through
// every path that renders it: seed.Render directly (the pre-seam path), and
// the hostos IncusOS backend. The behaviour-neutral claim of the seam rests on
// both matching the same files.
func TestIncusOSSeedGolden(t *testing.T) {
	paths := []struct {
		name   string
		render func(*testing.T, goldenCase) map[string][]byte
	}{
		{"seed.Render", renderViaSeed},
		{"hostos", renderViaHostOS},
	}

	for _, c := range goldenCases() {
		goldenDir := filepath.Join("testdata", "incusos", c.name)
		if *update {
			if err := os.MkdirAll(goldenDir, 0o750); err != nil {
				t.Fatal(err)
			}
			for name, b := range renderViaSeed(t, c) {
				if err := os.WriteFile(filepath.Join(goldenDir, name), b, 0o644); err != nil { //nolint:gosec // golden test fixtures, not secret
					t.Fatal(err)
				}
			}
		}
		want := readDir(t, goldenDir)

		for _, p := range paths {
			t.Run(c.name+"/"+p.name, func(t *testing.T) {
				got := p.render(t, c)
				for _, name := range seedFileNames {
					if !bytes.Equal(got[name], want[name]) {
						t.Errorf("%s differs from %s:\ngot:\n%s\nwant:\n%s", name, filepath.Join(goldenDir, name), got[name], want[name])
					}
				}
			})
		}
	}
}
