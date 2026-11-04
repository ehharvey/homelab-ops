package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiNetworksCmd = &cobra.Command{
	Use:   "networks",
	Short: "GET /networks: the synced networks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Networks(ctx)
		})
	},
}

func init() {
	apiCmd.AddCommand(apiNetworksCmd)
}
