package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiUnlockKeyFile string

var apiUnlockCmd = &cobra.Command{
	Use:   "unlock",
	Short: "POST /store/unlock: supply the store's key after a restart",
	Long: `unlock sends the key in --key-file, as init or rotate wrote it, to a
restarted web app whose store is locked. A key that doesn't decrypt the
store is refused (403), and the store file is left as it was.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		key, err := readStoreKeyFile("--key-file", apiUnlockKeyFile)
		if err != nil {
			return err
		}
		return runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Unlock(ctx, key)
		})
	},
}

func init() {
	apiUnlockCmd.Flags().StringVar(&apiUnlockKeyFile, "key-file", "", "file holding the store key (required)")
	apiCmd.AddCommand(apiUnlockCmd)
}
