package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/webappliance"
)

var (
	applianceSeedConfigDir string
	applianceSeedOutput    string
	applianceSeedImage     string
	applianceSeedReboot    string
	applianceSeedSSHKeys   string
	applianceSeedNoSecrets bool
	applianceSeedForce     bool
)

var renderApplianceSeedCmd = &cobra.Command{
	Use:   "render-appliance-seed",
	Short: "Render the web app appliance's NoCloud seed (user-data, meta-data) from a deployment config directory",
	Long: `render-appliance-seed renders the NoCloud seed that turns the generic
appliance image (make appliance-image, deploy/nixos/) into this operator's web
app appliance at first boot (docs/Deployment.md § The appliance). Copy
user-data and meta-data onto a FAT volume labelled CIDATA and boot the
appliance with it attached; remove it afterwards, since it holds secrets/.

user-data carries every file of --config-dir inline, with its mode, plus
deploy/compose.yaml with the web image pinned to --image, and the host
settings (--reboot-time, --ssh-authorized-keys). With --no-secrets it leaves
out exactly secrets/; the web app then starts once secrets/ is copied to
/etc/homelab-ops/web/secrets on the appliance, over SSH, so --no-secrets
needs --ssh-authorized-keys. Dot entries (.git/, .DS_Store) and editor
backups (name~) are left out, and named.

It checks the directory as the seed ships it, less what it leaves out, and
refuses one the appliance's web app would refuse at startup (deployconfig's
own checks, run with deploy/compose.yaml's environment); one whose
NETWORK_ADDRESSING_PATH or BASE_IMAGE_PATH doesn't name a file the seed
carries, as /etc/homelab-ops/web/<file>; and one without WIREGUARD_ENDPOINT:
the appliance serves the API only through the tunnel.

Without --ssh-authorized-keys the appliance has no shell at all: you confirm
it by the web app answering over the tunnel, roll back from the boot menu,
and change anything by reinstalling with a new seed.

The appliance reads a seed only at its first boot, so a re-rendered seed
changes nothing on a running box. The web image is never upgraded
automatically: to upgrade it, render the seed with a new --image and
reinstall the appliance with it (docs/Deployment.md § Upgrades and
rollback). With SSH, you can instead edit /etc/homelab-ops/compose.yaml on
the appliance and run docker compose pull && docker compose up -d there.`,
	Args: cobra.NoArgs,
	RunE: runRenderApplianceSeed,
}

func init() {
	f := renderApplianceSeedCmd.Flags()
	f.StringVar(&applianceSeedConfigDir, "config-dir", "", "the deployment config directory to carry, laid out as docs/Deployment.md describes (required)")
	f.StringVar(&applianceSeedOutput, "output", "./bootstrap-output/appliance-seed", "directory to write user-data and meta-data to (mode 0600: user-data holds secrets/)")
	f.StringVar(&applianceSeedImage, "image", "", "the web image, pinned by digest: <repo>@sha256:<hex>, or sha256:<hex> for ghcr.io/ehharvey/homelab-ops/web (default: deploy/compose.yaml's pin)")
	f.StringVar(&applianceSeedReboot, "reboot-time", webappliance.DefaultRebootTime, "HH:MM, the appliance's local time: when it upgrades each day and the start of the one-hour window in which it may reboot")
	f.StringVar(&applianceSeedSSHKeys, "ssh-authorized-keys", "", "an authorized_keys file of public keys allowed to log in as root over SSH; enables sshd and opens port 22 (default: SSH off)")
	f.BoolVar(&applianceSeedNoSecrets, "no-secrets", false, "leave out the config directory's secrets/; copy it in over SSH after boot (needs --ssh-authorized-keys), and the web app starts once it's there")
	f.BoolVar(&applianceSeedForce, "force", false, "overwrite an existing user-data and meta-data in --output")

	rootCmd.AddCommand(renderApplianceSeedCmd)
}

func runRenderApplianceSeed(cmd *cobra.Command, _ []string) error {
	if applianceSeedConfigDir == "" {
		return fmt.Errorf("--config-dir is required")
	}
	opts := webappliance.Options{
		ConfigDir:  applianceSeedConfigDir,
		NoSecrets:  applianceSeedNoSecrets,
		Image:      applianceSeedImage,
		RebootTime: applianceSeedReboot,
	}
	if applianceSeedSSHKeys != "" {
		b, err := os.ReadFile(applianceSeedSSHKeys) //nolint:gosec // G304: the path is operator-supplied via --ssh-authorized-keys
		if err != nil {
			return fmt.Errorf("--ssh-authorized-keys: %w", err)
		}
		keys, err := webappliance.ParseAuthorizedKeys(b)
		if err != nil {
			return fmt.Errorf("--ssh-authorized-keys %s: %w", applianceSeedSSHKeys, err)
		}
		opts.SSHAuthorizedKeys = keys
	}

	seed, err := webappliance.Render(opts)
	if err != nil {
		return err
	}
	if err := webappliance.Write(applianceSeedOutput, seed, applianceSeedForce); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	for _, rel := range seed.Skipped {
		_, _ = fmt.Fprintf(out, "skipped %s: dot entries and editor backups stay out of the seed\n", rel)
	}
	if len(opts.SSHAuthorizedKeys) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: no --ssh-authorized-keys, so the appliance will have no shell (no sshd, and root has no password). "+
			"Confirm it by the web app answering over the tunnel, roll back from the boot menu, and change anything by reinstalling with a new seed (docs/Deployment.md § The appliance).")
	}
	_, _ = fmt.Fprintf(out, "wrote %s/user-data and %s/meta-data\n", applianceSeedOutput, applianceSeedOutput)
	if applianceSeedNoSecrets {
		_, _ = fmt.Fprintln(out, "secrets/ left out: the web app starts once it is copied to /etc/homelab-ops/web/secrets on the appliance")
	} else {
		_, _ = fmt.Fprintln(out, "user-data holds secrets/ in plaintext: remove the CIDATA stick after the first boot")
	}
	return nil
}
