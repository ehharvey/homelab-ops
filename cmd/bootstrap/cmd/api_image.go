package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiImageOutput string

var apiImageCmd = &cobra.Command{
	Use:   "image <instance> --output <file|device>",
	Short: "GET /instances/{name}/image: stream the instance's seeded disk image to a file",
	Long: `image streams the instance's freshly built, seeded disk image to --output as
it arrives, never holding it in memory, so a multi-GB image can go straight
to a file or to install media. The image carries the node's seed, so a new
file is created mode 0600. A download cut short, or one that doesn't match
the size the web app announced, exits non-zero and removes the partial file
(a device is left as is). Pass - to write the image to stdout.

The web app builds the image per request, so the first byte can take a
while; --connect-timeout bounds only reaching the web app.`,
	Args: cobra.ExactArgs(1),
	RunE: runAPIImage,
}

func init() {
	apiImageCmd.Flags().StringVarP(&apiImageOutput, "output", "o", "", "file or device to stream the image to, or - for stdout (required)")
	apiCmd.AddCommand(apiImageCmd)
}

func runAPIImage(cmd *cobra.Command, args []string) error {
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
