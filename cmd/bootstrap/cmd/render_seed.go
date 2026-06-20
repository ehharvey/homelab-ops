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
	renderSeedOutputDir    string
	renderSeedForce        bool
	renderSeedForceInstall bool
	renderSeedForceReboot  bool
)

var renderSeedCmd = &cobra.Command{
	Use:   "render-seed",
	Short: "Render an IncusOS install seed (install.yaml, network.yaml, applications.yaml) from a fleet definition",
	Long: `render-seed reads a fleet definition YAML file containing exactly one
"kind: Network" and one "kind: Instance" document and renders the three
IncusOS seed files (install.yaml, network.yaml, applications.yaml) that
flasher-tool later bakes into a .img.`,
	RunE: runRenderSeed,
}

func init() {
	renderSeedCmd.Flags().StringVar(&renderSeedFile, "file", "", "path to the fleet definition YAML file (required)")
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

	if len(cfg.Networks) != 1 {
		return fmt.Errorf("expected exactly 1 Network, got %d", len(cfg.Networks))
	}
	if len(cfg.Instances) != 1 {
		return fmt.Errorf("expected exactly 1 Instance, got %d", len(cfg.Instances))
	}

	bundle, err := seed.Render(cfg.Networks[0], cfg.Instances[0], seed.Options{
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
	if _, err := fmt.Fprintf(out, "Wrote %s/install.yaml, %s/network.yaml, %s/applications.yaml\n",
		renderSeedOutputDir, renderSeedOutputDir, renderSeedOutputDir); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}
