package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeFakeFlasherToolBin(t *testing.T, dir string) string {
	t.Helper()

	script := "#!/bin/sh\n" +
		"image=\"\"\n" +
		"while [ \"$#\" -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--image\" ]; then image=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"printf 'seeded' >> \"$image\"\n"

	binPath := filepath.Join(dir, "fake-flasher-tool")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture, intentionally executable
		t.Fatalf("write fake flasher-tool: %v", err)
	}

	return binPath
}

func writeSeedFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"install.yaml", "network.yaml", "applications.yaml", "incus.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("k: v\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestBuildImageCommandHappyPath(t *testing.T) {
	dir := t.TempDir()
	seedDir := filepath.Join(dir, "seed")
	if err := os.MkdirAll(seedDir, 0o750); err != nil {
		t.Fatalf("mkdir seed dir: %v", err)
	}
	writeSeedFiles(t, seedDir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}
	outputImage := filepath.Join(dir, "out.img")
	binPath := writeFakeFlasherToolBin(t, dir)

	rootCmd.SetArgs([]string{
		"build-image",
		"--seed-dir", seedDir,
		"--image", baseImage,
		"--output", outputImage,
		"--flasher-tool-bin", binPath,
	})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	got, err := os.ReadFile(outputImage)
	if err != nil {
		t.Fatalf("read output image: %v", err)
	}
	if string(got) != "baseseeded" {
		t.Errorf("output image contents = %q, want %q", got, "baseseeded")
	}
}

func TestBuildImageCommandRequiresImageFlag(t *testing.T) {
	rootCmd.SetArgs([]string{"build-image", "--seed-dir", t.TempDir(), "--output", filepath.Join(t.TempDir(), "out.img")})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error when --image is missing, got nil")
	}
}

func TestBuildImageCommandMissingFlasherTool(t *testing.T) {
	dir := t.TempDir()
	seedDir := filepath.Join(dir, "seed")
	if err := os.MkdirAll(seedDir, 0o750); err != nil {
		t.Fatalf("mkdir seed dir: %v", err)
	}
	writeSeedFiles(t, seedDir)

	baseImage := filepath.Join(dir, "base.img")
	if err := os.WriteFile(baseImage, []byte("base"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}

	rootCmd.SetArgs([]string{
		"build-image",
		"--seed-dir", seedDir,
		"--image", baseImage,
		"--output", filepath.Join(dir, "out.img"),
		"--flasher-tool-bin", "definitely-not-a-real-flasher-tool-binary",
	})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error for missing flasher-tool binary, got nil")
	}
}
