package cmd

import (
	"context"
	"errors"
	"fmt"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/sealed"
)

var apiInitKeyFile string

var apiInitCmd = &cobra.Command{
	Use:   "init",
	Short: "POST /store/init: start the web app's store under a newly generated key",
	Long: `init generates the web app store's key, an age X25519 identity
(AGE-SECRET-KEY-1...), writes it to --key-file (mode 0600; the file must not
exist yet), and sends it to a web app that has no store yet. The web app
holds the key in memory only, and encrypts its store file to it.

Keep the key file: after every restart the web app is locked until unlock
supplies it, and a lost key means reset, which discards the store's state.
A web app without STORE_PATH keeps its store in memory, so every start
begins empty and needs init again.

If the web app refuses, or the request never reaches it (no connection),
the generated key stays in --key-file; delete it before trying again. If
its answer is lost instead, keep the file: if lock-state reads unlocked,
the store is under it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		key, err := newStoreKeyFile("--key-file", apiInitKeyFile)
		if err != nil {
			return err
		}
		err = runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Init(ctx, key)
		})
		return keyVerbError(err, apiInitKeyFile, "if lock-state reads unlocked, the store is under it")
	},
}

// keyVerbError explains a failed init, rotate or reset to the operator,
// whose new key is in file. Two failures mean the key wasn't taken, so the
// file can go (and must, since the verbs create it exclusively): a 4xx
// refusal from the web app, and an error from before the request was
// written (apiclient.ErrNotSent: the tunnel or the connection never came
// up). Anything else (the connection dropped once the request was sent, a
// timeout, a 5xx) may have lost the answer to a verb that did succeed, and
// deleting the file then loses the only key that unlocks the store: so the
// file is kept, and ifLost says how to tell. nil stays nil.
func keyVerbError(err error, file, ifLost string) error {
	if err == nil {
		return nil
	}
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
		return fmt.Errorf("%w (the web app refused, so the key generated in %s wasn't taken; delete it before trying again)", err, file)
	}
	if errors.Is(err, apiclient.ErrNotSent) {
		return fmt.Errorf("%w (the request never reached the web app, so the key generated in %s wasn't taken; delete it before trying again)", err, file)
	}
	return fmt.Errorf("%w (the web app may have taken the key generated in %s before its answer was lost: keep the file; %s)", err, file, ifLost)
}

func init() {
	apiInitCmd.Flags().StringVar(&apiInitKeyFile, "key-file", "", "file to write the generated store key to; must not exist (required)")
	apiCmd.AddCommand(apiInitCmd)
}

// newStoreKeyFile generates a store key into path, which flag names, before
// any request: the key is on disk before the web app holds it, so a lost
// answer never loses the key.
func newStoreKeyFile(flag, path string) (*age.X25519Identity, error) {
	if path == "" {
		return nil, fmt.Errorf("%s is required: the file the generated store key is written to", flag)
	}
	key, err := sealed.WriteNewKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	return key, nil
}

// readStoreKeyFile reads the store key in path, which flag names.
func readStoreKeyFile(flag, path string) (*age.X25519Identity, error) {
	if path == "" {
		return nil, fmt.Errorf("%s is required: the file holding the store key", flag)
	}
	key, err := sealed.ReadKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flag, err)
	}
	return key, nil
}
