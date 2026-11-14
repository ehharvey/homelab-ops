package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiLockStateCmd = &cobra.Command{
	Use:   "lock-state",
	Short: "GET /store/state: whether the web app's store is initialized and unlocked",
	Long: `lock-state prints the web app's lock state, which it answers even while
locked: {"state": "no_store" | "locked" | "unlocked", "store": "file" |
"memory"}. no_store wants init, locked wants unlock (or, for a lost key,
reset).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.LockState(ctx)
		})
	},
}

func init() {
	apiCmd.AddCommand(apiLockStateCmd)
}
