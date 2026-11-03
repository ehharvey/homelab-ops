package hostos

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/flasher"
)

// TestBackendsMatchConfig keeps the registry and config.Validate in step: an
// os config.Validate accepts must have a backend, and a backend must serve an
// os config.Validate accepts.
func TestBackendsMatchConfig(t *testing.T) {
	for _, hostOS := range config.HostOSes() {
		_, err := For(hostOS)
		if implemented := config.HostOSImplemented(hostOS); implemented != (err == nil) {
			t.Errorf("os %q: config.HostOSImplemented = %v, but For error = %v", hostOS, implemented, err)
		}
	}
	for hostOS, b := range backends {
		if b.OS() != hostOS {
			t.Errorf("backends[%q].OS() = %q", hostOS, b.OS())
		}
		if !config.HostOSImplemented(hostOS) {
			t.Errorf("backend %q is registered but config.HostOSImplemented(%q) is false", hostOS, hostOS)
		}
	}
}

func TestForInstanceDefaultsToIncusOS(t *testing.T) {
	b, err := ForInstance(config.Instance{Name: "node0"})
	if err != nil {
		t.Fatalf("ForInstance: %v", err)
	}
	if b.OS() != config.OSIncusOS {
		t.Errorf("ForInstance(no os).OS() = %q, want %q", b.OS(), config.OSIncusOS)
	}
}

// TestRenderConfigRejectsUnregisteredOS covers a config that skipped
// config.Validate: rendering fails rather than falling back to IncusOS.
func TestRenderConfigRejectsUnregisteredOS(t *testing.T) {
	inst := goldenInstance()
	inst.OS = config.OSTalos
	_, err := RenderConfig(ConfigInput{Network: goldenNetwork(), Instance: inst, ClientCertPEM: readFixture(t, "client.crt")})
	if err == nil || !strings.Contains(err.Error(), `no host OS backend for "talos"`) {
		t.Fatalf("RenderConfig(os talos) error = %v, want no backend", err)
	}
}

func TestIncusOSValidateInstance(t *testing.T) {
	b, err := For(config.OSIncusOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateInstance(goldenInstance()); err != nil {
		t.Errorf("ValidateInstance(good) = %v", err)
	}
	bad := goldenInstance()
	bad.Disk = "raid"
	if err := b.ValidateInstance(bad); err == nil || !strings.Contains(err.Error(), `disk "raid" not supported`) {
		t.Errorf("ValidateInstance(disk raid) = %v, want the disk rejection", err)
	}
}

func TestWriteFilesRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	files := []File{{Name: "a.yaml", Data: []byte("one\n")}}
	if err := WriteFiles(dir, files, false); err != nil {
		t.Fatalf("first WriteFiles: %v", err)
	}
	if err := WriteFiles(dir, files, false); err == nil || !strings.Contains(err.Error(), "already exists (use --force to overwrite)") {
		t.Fatalf("second WriteFiles = %v, want an overwrite refusal", err)
	}
	files[0].Data = []byte("two\n")
	if err := WriteFiles(dir, files, true); err != nil {
		t.Fatalf("forced WriteFiles: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.yaml")) //nolint:gosec // test temp dir
	if err != nil || string(got) != "two\n" {
		t.Fatalf("a.yaml = %q, %v; want the forced write", got, err)
	}
}

// TestWriteFilesChecksAllBeforeWriting pins that a refusal leaves the dir as
// it was: with only the last file already present, none of the others are
// written.
func TestWriteFilesChecksAllBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.yaml"), []byte("old\n"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	files := []File{{Name: "a.yaml", Data: []byte("a\n")}, {Name: "b.yaml", Data: []byte("b\n")}, {Name: "c.yaml", Data: []byte("c\n")}}
	if err := WriteFiles(dir, files, false); err == nil || !strings.Contains(err.Error(), "c.yaml already exists") {
		t.Fatalf("WriteFiles = %v, want a refusal naming c.yaml", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dir holds %d files after a refusal, want only the pre-existing c.yaml", len(entries))
	}
}

// TestIncusOSRenderImageInjectsGoldenSeed drives the IncusOS image path with a
// fake flasher-tool that keeps the seed tar it's handed, and checks that tar
// is the one flasher.BuildSeedTar makes from the golden seed files.
func TestIncusOSRenderImageInjectsGoldenSeed(t *testing.T) {
	dir := t.TempDir()
	keptTar := filepath.Join(dir, "kept-seed.tar")
	tool := filepath.Join(dir, "flasher-tool")
	script := "#!/bin/sh\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    --image) image=\"$2\" ;;\n" +
		"    --seed) cp \"$2\" " + keptTar + " ;;\n" +
		"  esac\n" +
		"  shift\n" +
		"done\n" +
		"printf _seeded >> \"$image\"\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, intentionally executable
		t.Fatal(err)
	}
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}

	goldenDir := filepath.Join("testdata", "incusos", "wireguard")
	b, err := For(config.OSIncusOS)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", "node0.img")
	var logs bytes.Buffer
	err = b.RenderImage(context.Background(), ImageInput{
		ConfigDir:   goldenDir,
		BaseImage:   base,
		OutputImage: out,
		ToolPath:    tool,
		Stdout:      &logs,
		Stderr:      &logs,
	})
	if err != nil {
		t.Fatalf("RenderImage: %v\n%s", err, logs.String())
	}

	if img, err := os.ReadFile(out); err != nil || string(img) != "base_seeded" { //nolint:gosec // test temp dir
		t.Errorf("output image = %q, %v; want the base image plus the fake tool's mark", img, err)
	}
	wantTar, err := flasher.BuildSeedTar(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	gotTar, err := os.ReadFile(keptTar) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatalf("fake flasher-tool kept no seed tar: %v", err)
	}
	if !bytes.Equal(gotTar, wantTar) {
		t.Errorf("seed tar handed to flasher-tool differs from flasher.BuildSeedTar(%s)", goldenDir)
	}
}
