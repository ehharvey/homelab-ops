package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiRotateFlags struct {
	keyFile, newKeyFile string
}

var apiRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "POST /store/rotate: re-encrypt the store under a newly generated key",
	Long: `rotate generates a new store key into --new-key-file (mode 0600; the
file must not exist yet) and has the unlocked web app re-encrypt its store
from the key in --key-file, which must be the one it holds, to the new one.
The store's state is kept; the old key no longer unlocks it.

If the web app refuses, or the request never reaches it, the new key stays
in --new-key-file and the old key still unlocks; delete the new file before
trying again. If its answer is lost instead, the store may be under either
key: keep both files.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		old, err := readStoreKeyFile("--key-file", apiRotateFlags.keyFile)
		if err != nil {
			return err
		}
		next, err := newStoreKeyFile("--new-key-file", apiRotateFlags.newKeyFile)
		if err != nil {
			return err
		}
		err = runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Rotate(ctx, old, next)
		})
		return keyVerbError(err, apiRotateFlags.newKeyFile, fmt.Sprintf(
			"keep --key-file's too: the store is under one of the two, and rotate with --key-file %s succeeds only if it is under the new one", apiRotateFlags.newKeyFile))
	},
}

func init() {
	f := apiRotateCmd.Flags()
	f.StringVar(&apiRotateFlags.keyFile, "key-file", "", "file holding the store's current key (required)")
	f.StringVar(&apiRotateFlags.newKeyFile, "new-key-file", "", "file to write the generated new key to; must not exist (required)")
	apiCmd.AddCommand(apiRotateCmd)
}
