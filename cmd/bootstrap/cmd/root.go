package cmd

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Bootstrap CLI for provisioning node #0 (offline, pre-web-app)",
	Long: `bootstrap is a standalone, offline-first CLI used once per homelab to get
node #0 up: generating a trusted cert/key, rendering an IncusOS install seed,
and invoking flasher-tool to produce a flashable .img.`,
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
