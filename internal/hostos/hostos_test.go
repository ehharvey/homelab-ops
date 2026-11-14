package hostos

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// TestIncusOSRenderSeedMediaIsSeedDataISO checks the IncusOS seed media is an
// ISO 9660 volume labelled SEED_DATA holding the four seed files at its root
// (internal/iso9660's own tests read the format back in full). It runs over
// every golden seed, which TestIncusOSSeedGolden pins to RenderConfig's output,
// so a file RenderConfig gains whose name plain ISO 9660 can't hold (a "-",
// say) fails here rather than in seed-media.
func TestIncusOSRenderSeedMediaIsSeedDataISO(t *testing.T) {
	for _, tc := range []string{"dhcp", "static", "wireguard"} {
		t.Run(tc, func(t *testing.T) { checkSeedDataISO(t, readDir(t, filepath.Join("testdata", "incusos", tc))) })
	}
}

func checkSeedDataISO(t *testing.T, golden map[string][]byte) {
	t.Helper()
	var files []File
	for name, data := range golden {
		files = append(files, File{Name: name, Data: data})
	}
	if len(files) != 4 {
		t.Fatalf("golden seed has %d files, want 4", len(files))
	}

	var buf bytes.Buffer
	if err := RenderSeedMedia(IncusOS, files, &buf); err != nil {
		t.Fatalf("RenderSeedMedia: %v", err)
	}
	img := buf.Bytes()
	const sector = 2048
	if len(img) < 17*sector || string(img[16*sector+1:16*sector+6]) != "CD001" {
		t.Fatalf("seed media has no ISO 9660 volume descriptor at sector 16")
	}
	if got := strings.TrimRight(string(img[16*sector+40:16*sector+72]), " "); got != IncusOSSeedVolumeID {
		t.Errorf("volume ID = %q, want %q", got, IncusOSSeedVolumeID)
	}
	for name, data := range golden {
		if id := strings.ToUpper(name) + ";1"; !bytes.Contains(img, []byte(id)) {
			t.Errorf("no root directory record %s", id)
		}
		found := false
		for off := 0; off+len(data) <= len(img); off += sector {
			if bytes.Equal(img[off:off+len(data)], data) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s's contents aren't on the image at a sector boundary", name)
		}
	}
}

// TestRenderSeedMediaNamesUnsupportedOS covers both ways seed media is
// refused: an os with no backend, and a backend whose OS has no separate
// seed media (one that isn't a SeedMediaRenderer, as Talos's would be).
func TestRenderSeedMediaNamesUnsupportedOS(t *testing.T) {
	var buf bytes.Buffer
	if _, err := For(config.OSTalos); err == nil || !strings.Contains(err.Error(), `"talos"`) {
		t.Errorf("For(talos) error = %v, want one naming talos", err)
	}

	err := RenderSeedMedia(noSeedMediaBackend{os: config.OSDebian}, sampleFiles, &buf)
	if !errors.Is(err, ErrNoSeedMedia) || !strings.Contains(err.Error(), `"debian"`) {
		t.Errorf("RenderSeedMedia(no seed media) = %v, want ErrNoSeedMedia naming debian", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused RenderSeedMedia wrote %d bytes", buf.Len())
	}
}

// TestCheckSeedMedia: CheckSeedMedia answers from the backend's type alone.
// It refuses a backend with no seed media, naming its OS, and passes
// IncusOS, and it never renders: a renderer that would fail, or that
// counts its calls, is passed without being called.
func TestCheckSeedMedia(t *testing.T) {
	if err := CheckSeedMedia(IncusOS); err != nil {
		t.Errorf("CheckSeedMedia(IncusOS) = %v, want nil", err)
	}
	err := CheckSeedMedia(noSeedMediaBackend{os: config.OSDebian})
	if !errors.Is(err, ErrNoSeedMedia) || !strings.Contains(err.Error(), `"debian"`) {
		t.Errorf("CheckSeedMedia(no seed media) = %v, want ErrNoSeedMedia naming debian", err)
	}
	r := &countingRenderer{noSeedMediaBackend: noSeedMediaBackend{os: config.OSDebian}}
	if err := CheckSeedMedia(r); err != nil {
		t.Errorf("CheckSeedMedia(a SeedMediaRenderer) = %v, want nil", err)
	}
	if r.calls != 0 {
		t.Errorf("CheckSeedMedia rendered %d times, want 0", r.calls)
	}
}

var sampleFiles = []File{{Name: "install.yaml", Data: []byte("i")}}

// noSeedMediaBackend is a Backend whose OS has no separate seed media.
type noSeedMediaBackend struct {
	Backend
	os string
}

func (b noSeedMediaBackend) OS() string { return b.os }

// countingRenderer is a SeedMediaRenderer whose render always fails, as one
// that checked its files first would given none.
type countingRenderer struct {
	noSeedMediaBackend
	calls int
}

func (r *countingRenderer) RenderSeedMedia([]File, io.Writer) error {
	r.calls++
	return errors.New("no install.yaml")
}
