package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/hostos"
)

var apiSeedMediaOutput string

var apiSeedMediaCmd = &cobra.Command{
	Use:   "seed-media <instance> --output <file|device>",
	Short: "Write the instance's seed as separate install media, for use beside a stock install image",
	Long: `seed-media fetches the instance's seed (POST /instances/{name}/seed) and writes
it to --output as separate seed media, which the machine reads its install
config from while it boots its OS's stock, unmodified install image. Only the
seed, a few KB, crosses the tunnel, and the web app needs no base image
(docs/Deployment.md, "Where install media is built").

For IncusOS that's an ISO 9660 volume labelled SEED_DATA with the seed files
at its root. --output can be a file, for a VM's CD-ROM or to dd later, or a
block device such as a USB stick; boot the machine from the stock IncusOS
image with the media attached. The media is built in memory, and holds the
node's WireGuard private key and bootstrap credential. A file --output is
written as a mode-0600 file beside it and renamed over it, so it ends mode
0600 even over an existing file, and a symlinked --output replaces its
target; a device is written in place. Pass - to write it to stdout.

Remove the media once the install finishes. It still carries the install
config, so the installed system finds install media on its next boot and
stops there until the media is gone.`,
	Args: cobra.ExactArgs(1),
	RunE: runAPISeedMedia,
}

func init() {
	apiSeedMediaCmd.Flags().StringVarP(&apiSeedMediaOutput, "output", "o", "", "file or device to write the seed media to, or - for stdout (required)")
	apiCmd.AddCommand(apiSeedMediaCmd)
}

func runAPISeedMedia(cmd *cobra.Command, args []string) error {
	out := apiSeedMediaOutput
	if err := checkOutput(out); err != nil {
		return err
	}
	// A file --output (new, or an existing regular file) is staged beside
	// it and renamed over it, so it ends 0600 whatever its old mode; staged
	// before any request, so a directory that won't take the new file fails
	// before the seed is minted. A device, or stdout, is written in place.
	var staged *stagedOutput
	if out != "-" {
		target := resolveOutput(out)
		if info, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) || (err == nil && info.Mode().IsRegular()) {
			if staged, err = stageOutput(target); err != nil {
				return err
			}
			defer staged.remove()
		}
	}
	name := args[0]
	return withAPI(cmd, func(ctx context.Context, c *apiclient.Client) error {
		backend, files, err := fetchSeed(ctx, c, name, hostos.CheckSeedMedia)
		if err != nil {
			return err
		}
		// Rendered whole before --output is opened, so a refusal leaves an
		// existing file or device as it was.
		var media bytes.Buffer
		if err := hostos.RenderSeedMedia(backend, files, &media); err != nil {
			return fmt.Errorf("render %s's seed media: %w", name, err)
		}
		write := func(w io.Writer) error {
			if _, err := w.Write(media.Bytes()); err != nil {
				return fmt.Errorf("write seed media: %w", err)
			}
			// A USB stick is pulled soon after this returns: flush it.
			if f, ok := w.(*os.File); ok && out != "-" {
				if err := f.Sync(); err != nil {
					return fmt.Errorf("sync %s: %w", out, err)
				}
			}
			return nil
		}
		if staged != nil {
			err = writeStaged(staged, write)
		} else {
			err = writeOutput(cmd, out, write)
		}
		if err != nil {
			return err
		}
		where := apiSeedMediaOutput
		if where == "-" {
			where = "stdout"
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s's %s seed media to %s (%d bytes).\n"+ //nolint:errcheck // progress note on stderr
			"Boot the machine from the stock %s install image with this media attached.\n"+
			"Remove the media once the install finishes: left attached, the installed system finds install media on its next boot and stops.\n",
			name, backend.OS(), where, media.Len(), backend.OS())
		return nil
	})
}
