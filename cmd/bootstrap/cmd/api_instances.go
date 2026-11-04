package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiInstancesCmd = &cobra.Command{
	Use:   "instances",
	Short: "GET /instances: the synced instances and their assigned addresses",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Instances(ctx)
		})
	},
}

func init() {
	apiCmd.AddCommand(apiInstancesCmd)
}
