package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "POST /sync: pull the config repo now",
	Long: `sync asks the web app to pull its config repo now and prints the synced
commit, counts and diff counts. A failed pull or an invalid fleet is a 502
from the web app, so this exits non-zero with its message; the web app's log
holds the detail.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Sync(ctx)
		})
	},
}

func init() {
	apiCmd.AddCommand(apiSyncCmd)
}
