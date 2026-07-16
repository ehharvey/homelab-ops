package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

var (
	renderSeedFile         string
	renderSeedCertPath     string
	renderSeedOutputDir    string
	renderSeedForce        bool
	renderSeedForceInstall bool
	renderSeedForceReboot  bool
)

var renderSeedCmd = &cobra.Command{
	Use:   "render-seed",
	Short: "Render an IncusOS install seed (install.yaml, network.yaml, applications.yaml, incus.yaml) from a fleet definition",
	Long: `render-seed reads a fleet definition YAML file containing exactly one
"kind: Network" and one "kind: Instance" document and renders the four
IncusOS seed files (install.yaml, network.yaml, applications.yaml,
incus.yaml) that flasher-tool later bakes into a .img. incus.yaml preseeds
Incus to trust --cert (gen-cert's client.crt) on first boot, so run gen-cert
before render-seed.`,
	RunE: runRenderSeed,
}

func init() {
	renderSeedCmd.Flags().StringVar(&renderSeedFile, "file", "", "path to the fleet definition YAML file (required)")
	renderSeedCmd.Flags().StringVar(&renderSeedCertPath, "cert", "./bootstrap-output/cert/client.crt", "path to the bootstrap client certificate (gen-cert's output) to preseed as a trusted Incus client cert")
	renderSeedCmd.Flags().StringVar(&renderSeedOutputDir, "output-dir", "./bootstrap-output/seed", "directory to write the seed files to")
	renderSeedCmd.Flags().BoolVar(&renderSeedForce, "force", false, "overwrite existing seed files in --output-dir")
	renderSeedCmd.Flags().BoolVar(&renderSeedForceInstall, "force-install", false, "set install.yaml's force_install flag")
	renderSeedCmd.Flags().BoolVar(&renderSeedForceReboot, "force-reboot", false, "set install.yaml's force_reboot flag")

	rootCmd.AddCommand(renderSeedCmd)
}

func runRenderSeed(cmd *cobra.Command, _ []string) error {
	if renderSeedFile == "" {
		return fmt.Errorf("--file is required")
	}

	f, err := os.Open(renderSeedFile) //nolint:gosec // path is operator-supplied via --file, not untrusted external input
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file, nothing to flush

	cfg, err := config.Parse(f)
	if err != nil {
		return fmt.Errorf("parse config file: %w", err)
	}

	// Parse only checks structure/syntax; reject semantically invalid
	// addressing (gateway/range/static_ip not adding up) before rendering a
	// seed off it. This is the same config.Validate pass the web app runs in
	// server.SyncOnce — seed.Render itself no longer re-validates.
	if issues := config.Validate(cfg); !issues.Empty() {
		return fmt.Errorf("invalid config: %w", issues)
	}

	if len(cfg.Networks) != 1 {
		return fmt.Errorf("expected exactly 1 Network, got %d", len(cfg.Networks))
	}
	if len(cfg.Instances) != 1 {
		return fmt.Errorf("expected exactly 1 Instance, got %d", len(cfg.Instances))
	}

	certPEM, err := os.ReadFile(renderSeedCertPath) //nolint:gosec // path is operator-supplied via --cert, not untrusted external input
	if err != nil {
		return fmt.Errorf("read client cert (run gen-cert first, or pass --cert): %w", err)
	}

	// No WireGuard config for the bootstrap CLI's node #0: the web app
	// doesn't exist yet at bootstrap time to hold the other end of the
	// tunnel (see docs/Architecture.md's Flow A vs. Flow B) — WireGuard is
	// steady-state-provisioning-only, wired up by the web app itself.
	bundle, err := seed.Render(cfg.Networks[0], cfg.Instances[0], certPEM, nil, seed.Options{
		ForceInstall: renderSeedForceInstall,
		ForceReboot:  renderSeedForceReboot,
	})
	if err != nil {
		return fmt.Errorf("render seed: %w", err)
	}

	if err := seed.Write(renderSeedOutputDir, bundle, renderSeedForce); err != nil {
		return fmt.Errorf("write seed: %w", err)
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Wrote %s/install.yaml, %s/network.yaml, %s/applications.yaml, %s/incus.yaml\n",
		renderSeedOutputDir, renderSeedOutputDir, renderSeedOutputDir, renderSeedOutputDir); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}
