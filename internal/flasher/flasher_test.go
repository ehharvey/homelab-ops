package flasher

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeSeedDir(t *testing.T, dir string) {
	t.Helper()
	for name, content := range map[string]string{
		"install.yaml":      "force_install: false\n",
		"network.yaml":      "interfaces: []\n",
		"applications.yaml": "applications: []\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestBuildSeedTarHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeSeedDir(t, dir)

	data, err := BuildSeedTar(dir)
	if err != nil {
		t.Fatalf("BuildSeedTar: %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(data))
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		got[hdr.Name] = string(contents)
	}

	for _, name := range seedFiles {
		if _, ok := got[name]; !ok {
			t.Errorf("expected tar entry %q, not found (entries: %v)", name, got)
		}
	}
	if len(got) != len(seedFiles) {
		t.Errorf("expected exactly %d entries, got %d: %v", len(seedFiles), len(got), got)
	}
}

func TestBuildSeedTarMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.yaml"), []byte("force_install: false\n"), 0o644); err != nil {
		t.Fatalf("write install.yaml: %v", err)
	}

	if _, err := BuildSeedTar(dir); err == nil {
		t.Fatal("expected error for missing network.yaml/applications.yaml, got nil")
	}
}

// writeFakeFlasherTool writes an executable shell script at dir/flasher-tool
// that records its argv to argvLog and exits with exitCode. If injectMarker
// is non-empty, it appends that string to the file passed via --image. If
// deleteImage is true, it removes the file passed via --image before
// exiting (used to test Run's post-condition check).
func writeFakeFlasherTool(t *testing.T, dir string, exitCode int, injectMarker string, deleteImage bool) string {
	t.Helper()

	argvLog := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"echo \"$@\" > " + argvLog + "\n" +
		"image=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--image\" ]; then image=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n"

	if injectMarker != "" {
		script += "printf '" + injectMarker + "' >> \"$image\"\n"
	}
	if deleteImage {
		script += "rm -f \"$image\"\n"
	}
	script += "echo fake-flasher-tool-stderr-output 1>&2\n"
	script += "exit " + strconv.Itoa(exitCode) + "\n"

	binPath := filepath.Join(dir, "flasher-tool")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, intentionally executable
		t.Fatalf("write fake flasher-tool: %v", err)
	}

	return binPath
}

func TestRunHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeSeedDir(t, dir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base-image-contents"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}

	binPath := writeFakeFlasherTool(t, dir, 0, "_seeded", false)
	outputImage := filepath.Join(dir, "out", "node0.img")

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), Options{
		SeedDir:     dir,
		BaseImage:   baseImage,
		OutputImage: outputImage,
		BinPath:     binPath,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v\nstderr: %s", err, stderr.String())
	}

	got, err := os.ReadFile(outputImage)
	if err != nil {
		t.Fatalf("read output image: %v", err)
	}
	if string(got) != "base-image-contents_seeded" {
		t.Errorf("output image contents = %q, want %q", got, "base-image-contents_seeded")
	}

	base, err := os.ReadFile(baseImage)
	if err != nil {
		t.Fatalf("read base image: %v", err)
	}
	if string(base) != "base-image-contents" {
		t.Errorf("base image was mutated: %q", base)
	}

	argv, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatalf("read argv.log: %v", err)
	}
	if !strings.Contains(string(argv), "--image "+outputImage) {
		t.Errorf("argv = %q, expected --image %s", argv, outputImage)
	}
	if !strings.Contains(string(argv), "--seed ") {
		t.Errorf("argv = %q, expected --seed flag", argv)
	}
}

func TestRunRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	writeSeedDir(t, dir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}
	outputImage := filepath.Join(dir, "out.img")
	if err := os.WriteFile(outputImage, []byte("existing"), 0o644); err != nil {
		t.Fatalf("write existing output image: %v", err)
	}

	binPath := writeFakeFlasherTool(t, dir, 0, "", false)

	err := Run(context.Background(), Options{
		SeedDir:     dir,
		BaseImage:   baseImage,
		OutputImage: outputImage,
		BinPath:     binPath,
	})
	if err == nil {
		t.Fatal("expected error for existing output image without --force, got nil")
	}
}

func TestRunSubprocessFailureSurfacesStderr(t *testing.T) {
	dir := t.TempDir()
	writeSeedDir(t, dir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}
	outputImage := filepath.Join(dir, "out.img")

	binPath := writeFakeFlasherTool(t, dir, 1, "", false)

	var stderr bytes.Buffer
	err := Run(context.Background(), Options{
		SeedDir:     dir,
		BaseImage:   baseImage,
		OutputImage: outputImage,
		BinPath:     binPath,
		Stderr:      &stderr,
	})
	if err == nil {
		t.Fatal("expected error for non-zero flasher-tool exit, got nil")
	}
	if !strings.Contains(stderr.String(), "fake-flasher-tool-stderr-output") {
		t.Errorf("stderr = %q, expected subprocess stderr to be visible", stderr.String())
	}
}

func TestRunDetectsUnmodifiedOutput(t *testing.T) {
	dir := t.TempDir()
	writeSeedDir(t, dir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}
	outputImage := filepath.Join(dir, "out.img")

	// Exits 0 but deletes the output image, simulating a misbehaving
	// flasher-tool that reports success without actually injecting.
	binPath := writeFakeFlasherTool(t, dir, 0, "", true)

	err := Run(context.Background(), Options{
		SeedDir:     dir,
		BaseImage:   baseImage,
		OutputImage: outputImage,
		BinPath:     binPath,
	})
	if err == nil {
		t.Fatal("expected error when output image is missing after a successful exit, got nil")
	}
}

func TestRunMissingBinary(t *testing.T) {
	dir := t.TempDir()
	writeSeedDir(t, dir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}

	err := Run(context.Background(), Options{
		SeedDir:     dir,
		BaseImage:   baseImage,
		OutputImage: filepath.Join(dir, "out.img"),
		BinPath:     "definitely-not-a-real-flasher-tool-binary",
	})
	if err == nil {
		t.Fatal("expected error for missing flasher-tool binary, got nil")
	}
	if !strings.Contains(err.Error(), "go install") {
		t.Errorf("error = %q, expected actionable install hint", err.Error())
	}
}
