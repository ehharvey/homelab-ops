package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
)

var apiResetFlags struct {
	newKeyFile string
	yes        bool
}

var apiResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "POST /store/reset: for a lost key, set the store aside and start empty",
	Long: `reset is for a lost store key only. The locked web app moves its store
file aside (next to it, renamed *.reset-<time>; never deleted) and starts an
empty store under a key generated into --new-key-file (mode 0600; the file
must not exist yet).

Everything in the store is left behind: the instances' credentials and their
address history. The web app's WireGuard identity is deployment config, so
the tunnel survives, but existing nodes can't be managed until they are
re-enrolled; the cluster keeps running.

It asks for confirmation (y/n) unless --yes is given. A web app whose store
is unlocked refuses it: use rotate.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if apiResetFlags.newKeyFile == "" {
			return errors.New("--new-key-file is required: the file the generated store key is written to")
		}
		if !apiResetFlags.yes {
			ok, err := confirm(cmd, "Reset discards the web app's store (it is moved aside, not deleted) and starts empty. Continue? [y/N] ")
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("reset not confirmed; nothing was changed")
			}
		}
		next, err := newStoreKeyFile("--new-key-file", apiResetFlags.newKeyFile)
		if err != nil {
			return err
		}
		err = runAPIJSON(cmd, func(ctx context.Context, c *apiclient.Client) ([]byte, error) {
			return c.Reset(ctx, next)
		})
		return keyVerbError(err, apiResetFlags.newKeyFile, "if lock-state reads unlocked, the store was reset under it")
	},
}

func init() {
	f := apiResetCmd.Flags()
	f.StringVar(&apiResetFlags.newKeyFile, "new-key-file", "", "file to write the generated new key to; must not exist (required)")
	f.BoolVar(&apiResetFlags.yes, "yes", false, "don't ask for confirmation")
	apiCmd.AddCommand(apiResetCmd)
}

// confirm prints prompt to stderr and reads one line from stdin: y or yes
// (any case) is a yes; anything else, end of input included, is a no.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), prompt); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
