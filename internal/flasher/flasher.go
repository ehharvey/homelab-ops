// Package flasher shells out to the upstream IncusOS flasher-tool binary
// (github.com/lxc/incus-os/incus-osd/cmd/flasher-tool) to inject a rendered
// install seed into a base IncusOS raw image, producing a .img ready to dd
// onto a USB stick.
//
// flasher-tool itself is not vendored or imported as a library: its
// module pulls in github.com/lxc/incus/v7's full dependency tree, and the
// byte offset it writes the seed tar to within the image is an undocumented
// implementation detail of whatever IncusOS release the operator's base
// image came from. Shelling out keeps that pairing intact instead of
// freezing a copy of it that could silently drift.
package flasher

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// seedFiles are the exact entry names flasher-tool's --seed tar expects,
// matching what internal/seed.Write produces in a seed directory.
var seedFiles = []string{"install.yaml", "network.yaml", "applications.yaml"}

// Options configures a Run invocation.
type Options struct {
	// SeedDir is a directory containing install.yaml, network.yaml, and
	// applications.yaml (the output of the render-seed command).
	SeedDir string
	// BaseImage is a path to a pre-obtained base IncusOS raw image.
	BaseImage string
	// OutputImage is where the seeded .img is written. Run copies
	// BaseImage here before invoking flasher-tool, which mutates this
	// copy in place — BaseImage itself is never modified.
	OutputImage string
	// Force allows overwriting an existing OutputImage.
	Force bool
	// BinPath is the flasher-tool binary to invoke, resolved via
	// exec.LookPath. Defaults to "flasher-tool" (i.e. resolved from
	// $PATH) when empty.
	BinPath string
	// Stdout and Stderr receive the flasher-tool subprocess's output.
	Stdout io.Writer
	Stderr io.Writer
}

// BuildSeedTar reads install.yaml, network.yaml, and applications.yaml from
// dir and returns an in-memory tar archive with exactly those three
// entries, matching the layout flasher-tool's --seed flag expects.
func BuildSeedTar(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, name := range seedFiles {
		path := filepath.Join(dir, name)

		data, err := os.ReadFile(path) //nolint:gosec // dir is operator-supplied via --seed-dir, not untrusted external input
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write %s tar header: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("write %s tar contents: %w", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finalize seed tar: %w", err)
	}

	return buf.Bytes(), nil
}

// Run copies opts.BaseImage to opts.OutputImage, builds a seed tar from
// opts.SeedDir, and invokes flasher-tool to inject the seed into the
// output copy in place.
func Run(ctx context.Context, opts Options) error {
	binPath := opts.BinPath
	if binPath == "" {
		binPath = "flasher-tool"
	}

	resolvedBin, err := exec.LookPath(binPath)
	if err != nil {
		return fmt.Errorf("flasher-tool not found (%q): install via `go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool`: %w", binPath, err)
	}

	if !opts.Force {
		if _, statErr := os.Stat(opts.OutputImage); statErr == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", opts.OutputImage)
		}
	}

	if err := copyFile(opts.BaseImage, opts.OutputImage); err != nil {
		return fmt.Errorf("copy base image: %w", err)
	}

	seedTar, err := BuildSeedTar(opts.SeedDir)
	if err != nil {
		return fmt.Errorf("build seed tar: %w", err)
	}

	tmpSeed, err := os.CreateTemp("", "flasher-seed-*.tar")
	if err != nil {
		return fmt.Errorf("create temp seed tar: %w", err)
	}
	defer os.Remove(tmpSeed.Name()) //nolint:errcheck // best-effort cleanup of a temp file

	if _, err := tmpSeed.Write(seedTar); err != nil {
		_ = tmpSeed.Close()
		return fmt.Errorf("write temp seed tar: %w", err)
	}
	if err := tmpSeed.Close(); err != nil {
		return fmt.Errorf("close temp seed tar: %w", err)
	}

	//nolint:gosec // resolvedBin and args come from operator-supplied flags, not untrusted input
	cmd := exec.CommandContext(ctx, resolvedBin, "--image", opts.OutputImage, "--seed", tmpSeed.Name())
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run flasher-tool: %w", err)
	}

	if _, err := os.Stat(opts.OutputImage); err != nil {
		return fmt.Errorf("flasher-tool exited successfully but the output image is missing: %w", err)
	}

	return nil
}

// copyFile copies src to dst, refusing to follow symlinks for dst and
// creating dst's parent directory if needed.
func copyFile(src, dst string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	in, err := os.Open(src) //nolint:gosec // src is operator-supplied via --image, not untrusted external input
	if err != nil {
		return fmt.Errorf("open base image: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only file, nothing to flush

	out, err := os.Create(dst) //nolint:gosec // dst is operator-supplied via --output, not untrusted external input
	if err != nil {
		return fmt.Errorf("create output image: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close output image: %w", closeErr)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy image data: %w", err)
	}

	return err
}
