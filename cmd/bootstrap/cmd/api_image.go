package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/hostos"
)

var (
	apiImageOutput         string
	apiImageBaseImage      string
	apiImageFlasherToolBin string
)

var apiImageCmd = &cobra.Command{
	Use:   "image <instance> --output <file|device> [--base-image <file>]",
	Short: "Get the instance's seeded disk image: streamed from the web app, or built here",
	Long: `image writes the instance's seeded disk image to --output.

By default the web app builds it (GET /instances/{name}/image) and image
streams it to --output as it arrives, never holding it in memory, so a
multi-GB image can go straight to a file or to install media. The web app
builds it per request, so the first byte can take a while; --connect-timeout
bounds only reaching the web app. That route needs the web app's
BASE_IMAGE_PATH, and answers 503 without it.

With --base-image, image builds it here instead: it fetches only the seed (a
few KB) over the tunnel and injects it into a copy of --base-image, as
build-image does, so no image crosses the network and the web app needs no
base image (docs/Deployment.md, "Where install media is built"). For IncusOS
that runs flasher-tool (--flasher-tool-bin) on the base image 'make
incusos-base' caches. The seed files flasher-tool reads, and the seed tar
it's handed, sit in a private temporary directory, removed when image exits.
--output must then be a file, and not --base-image itself: dd it to install
media afterwards. It's built in a mode-0600 file beside --output and renamed
over it, so it ends mode 0600 even over an existing file. A symlinked
--output is written through to its target.

The image carries the node's seed, so a new file is created mode 0600. A
download cut short, or one that doesn't match the size the web app
announced, exits non-zero and removes the partial file (a device is left as
is), as does a failed local build. Pass - to stream the web app's image to
stdout.`,
	Args: cobra.ExactArgs(1),
	RunE: runAPIImage,
}

func init() {
	f := apiImageCmd.Flags()
	f.StringVarP(&apiImageOutput, "output", "o", "", "file or device to write the image to, or - for stdout (required; a file with --base-image)")
	f.StringVar(&apiImageBaseImage, "base-image", "", "build the image here from this base OS image and the downloaded seed, rather than streaming the web app's")
	f.StringVar(&apiImageFlasherToolBin, "flasher-tool-bin", "flasher-tool", "with --base-image: the flasher-tool binary to run (resolved via $PATH unless it contains a slash)")
	apiCmd.AddCommand(apiImageCmd)
}

func runAPIImage(cmd *cobra.Command, args []string) error {
	if apiImageBaseImage != "" {
		return runAPIImageLocal(cmd, args[0])
	}
	if cmd.Flags().Changed("flasher-tool-bin") {
		return errors.New("--flasher-tool-bin needs --base-image: without it the web app builds the image")
	}
	if err := checkOutput(apiImageOutput); err != nil {
		return err
	}
	return withAPI(cmd, func(ctx context.Context, c *apiclient.Client) error {
		dl, err := c.Image(ctx, args[0])
		if err != nil {
			return err
		}
		defer dl.Body.Close() //nolint:errcheck // read-only response, nothing to flush
		var n int64
		if err := writeOutput(cmd, apiImageOutput, func(w io.Writer) error {
			var err error
			n, err = apiclient.CopyDownload(w, dl)
			return err
		}); err != nil {
			return err
		}
		if apiImageOutput != "-" {
			fmt.Fprintf(cmd.ErrOrStderr(), "saved %s's image to %s (%d bytes)\n", args[0], apiImageOutput, n) //nolint:errcheck // progress note on stderr
		}
		return nil
	})
}

// runAPIImageLocal is image --base-image: fetch the seed, write it to a 0700
// temporary directory for the image tool, and build the image at --output
// with the instance's host-OS backend (hostos.Backend.RenderImage).
func runAPIImageLocal(cmd *cobra.Command, name string) error {
	out := apiImageOutput
	if out == "-" {
		return errors.New("--output - needs the web app's image: a local build (--base-image) writes a file")
	}
	if err := checkOutput(out); err != nil {
		return err
	}
	// The build is renamed over --output: resolve a symlink first, so it
	// replaces the link's target, as the streamed path writes through it.
	out = resolveOutput(out)
	outInfo, err := os.Stat(out)
	if err == nil && !outInfo.Mode().IsRegular() {
		return fmt.Errorf("--output %s isn't a regular file: a local build (--base-image) writes a file; dd it to the device afterwards", out)
	}
	baseInfo, err := os.Stat(apiImageBaseImage)
	if err != nil {
		return fmt.Errorf("--base-image: %w", err)
	} else if !baseInfo.Mode().IsRegular() {
		return fmt.Errorf("--base-image %s isn't a regular file", apiImageBaseImage)
	}
	// The seeded image must not replace the base it's copied from: the base
	// is shared by every node's build, and would then carry this node's
	// secrets. SameFile also catches a hard link; a --output that doesn't
	// exist yet can't be the existing base.
	if outInfo != nil && os.SameFile(outInfo, baseInfo) {
		return fmt.Errorf("--output %s is --base-image %s: the base image is never modified; write the image elsewhere", apiImageOutput, apiImageBaseImage)
	}
	// Found before any request, so a missing tool doesn't fail only after
	// POST .../seed has stored the node's credential.
	if _, err := exec.LookPath(apiImageFlasherToolBin); err != nil {
		return fmt.Errorf("--flasher-tool-bin %q: %w", apiImageFlasherToolBin, err)
	}
	// Built beside --output, then renamed over it (stagedOutput). Created
	// before any request too, so a directory that won't take a new file
	// fails before the credential is minted.
	staged, err := stageOutput(out)
	if err != nil {
		return err
	}
	defer staged.remove()

	return withAPI(cmd, func(ctx context.Context, c *apiclient.Client) error {
		backend, files, err := fetchSeed(ctx, c, name, nil)
		if err != nil {
			return err
		}

		// MkdirTemp creates the directory 0700, so the seed's secrets, the
		// files and the image tool's seed tar (TempDir), are readable only by
		// this user while the image tool runs.
		dir, err := os.MkdirTemp("", "bootstrap-seed-*")
		if err != nil {
			return fmt.Errorf("create seed directory: %w", err)
		}
		defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp dir
		if err := hostos.WriteFiles(dir, files, false); err != nil {
			return err
		}

		// The image tool keeps the staged file's 0600 mode.
		err = backend.RenderImage(ctx, hostos.ImageInput{
			ConfigDir:   dir,
			BaseImage:   apiImageBaseImage,
			OutputImage: staged.partial,
			Force:       true, // the file stageOutput made
			ToolPath:    apiImageFlasherToolBin,
			TempDir:     dir,
			Stdout:      cmd.ErrOrStderr(),
			Stderr:      cmd.ErrOrStderr(),
		})
		if err != nil {
			return fmt.Errorf("build %s's image locally: %w", name, err)
		}
		if err := staged.commit(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "built %s's %s image locally at %s\n", name, backend.OS(), out) //nolint:errcheck // progress note on stderr
		return nil
	})
}
