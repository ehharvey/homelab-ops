package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/hostos"
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

// fetchSeed is the download behind the client-side install media
// (api image --base-image, api seed-media; docs/Decisions.md §29): name's
// host-OS backend, and its seed as the files that backend's media carries.
//
// The backend is looked up first, from GET /instances, and check (when not
// nil) runs on it, so an OS with no backend, or a backend check refuses,
// fails before POST .../seed mints the node's bootstrap credential.
// GET /instances lists the same store rows the seed route renders from, and
// its OS is the row's, so the backend picked here is the one the server
// renders with. Today that's always IncusOS: the store keeps no os column
// and refuses to sync any other os (internal/store), so when the column
// lands it reaches this lookup through that same listing.
//
// The seed is read through apiclient.DecodeSeed alone, which is where a
// downloaded seed gets decrypted once downloads are encrypted (#198).
func fetchSeed(ctx context.Context, c *apiclient.Client, name string, check func(hostos.Backend) error) (hostos.Backend, []hostos.File, error) {
	list, err := c.Instances(ctx)
	if err != nil {
		return nil, nil, err
	}
	// The route encodes []config.Instance, so the whole instance reaches
	// ForInstance, whichever of its fields backend selection reads.
	var instances []config.Instance
	if err := json.Unmarshal(list, &instances); err != nil {
		return nil, nil, fmt.Errorf("decode instances: %w", err)
	}
	idx := slices.IndexFunc(instances, func(i config.Instance) bool { return i.Name == name })
	if idx < 0 {
		return nil, nil, fmt.Errorf("instance %q not found in the web app's synced config", name)
	}
	backend, err := hostos.ForInstance(instances[idx])
	if err != nil {
		return nil, nil, fmt.Errorf("instance %q: %w", name, err)
	}
	if check != nil {
		if err := check(backend); err != nil {
			return nil, nil, fmt.Errorf("instance %q: %w", name, err)
		}
	}

	body, err := c.Seed(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	files, err := apiclient.DecodeSeed(body)
	if err != nil {
		return nil, nil, err
	}
	return backend, files, nil
}
