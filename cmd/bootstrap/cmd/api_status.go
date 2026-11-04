package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "GET /status: whether the web app has synced, and at which commit",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Status(ctx)
		})
	},
}

func init() {
	apiCmd.AddCommand(apiStatusCmd)
}
