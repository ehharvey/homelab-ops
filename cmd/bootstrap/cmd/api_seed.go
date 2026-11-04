package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiSeedOutput string

var apiSeedCmd = &cobra.Command{
	Use:   "seed <instance> --output <file>",
	Short: "POST /instances/{name}/seed: save the instance's rendered seed to a file",
	Long: `seed saves the instance's rendered seed, the web app's JSON answer with one
YAML document per seed file (install_yaml, network_yaml, applications_yaml,
incus_yaml), to --output. The seed holds the node's WireGuard private key and
bootstrap credential, so a new file is created mode 0600, and --output has no
default: pass - to print it on stdout instead.`,
	Args: cobra.ExactArgs(1),
	RunE: runAPISeed,
}

func init() {
	apiSeedCmd.Flags().StringVarP(&apiSeedOutput, "output", "o", "", "file to save the seed to, or - for stdout (required)")
	apiCmd.AddCommand(apiSeedCmd)
}

func runAPISeed(cmd *cobra.Command, args []string) error {
	if err := checkOutput(apiSeedOutput); err != nil {
		return err
	}
	return withAPI(cmd, func(ctx context.Context, c *apiclient.Client) error {
		body, err := c.Seed(ctx, args[0])
		if err != nil {
			return err
		}
		if err := writeOutput(cmd, apiSeedOutput, func(w io.Writer) error { return writeWithNewline(w, body) }); err != nil {
			return err
		}
		if apiSeedOutput != "-" {
			fmt.Fprintf(cmd.ErrOrStderr(), "saved %s's seed to %s\n", args[0], apiSeedOutput) //nolint:errcheck // progress note on stderr
		}
		return nil
	})
}
