package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/config"
)

var parseConfigFile string

var parseConfigCmd = &cobra.Command{
	Use:   "parse-config",
	Short: "Parse a fleet definition YAML file and summarize what it contains",
	Long: `parse-config reads a k8s-style, multi-document YAML fleet definition
(one or more documents discriminated by "kind: Network" / "kind: Instance")
and prints a summary of the parsed Networks and Instances. It performs no
seed rendering — this only proves the file parses into in-memory objects.`,
	RunE: runParseConfig,
}

func init() {
	parseConfigCmd.Flags().StringVar(&parseConfigFile, "file", "", "path to the fleet definition YAML file (required)")

	rootCmd.AddCommand(parseConfigCmd)
}

func runParseConfig(cmd *cobra.Command, _ []string) error {
	if parseConfigFile == "" {
		return fmt.Errorf("--file is required")
	}

	f, err := os.Open(parseConfigFile) //nolint:gosec // path is operator-supplied via --file, not untrusted external input
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file, nothing to flush

	cfg, err := config.Parse(f)
	if err != nil {
		return fmt.Errorf("parse config file: %w", err)
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Parsed %d Network(s), %d Instance(s):\n", len(cfg.Networks), len(cfg.Instances)); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	for _, n := range cfg.Networks {
		if _, err := fmt.Fprintf(out, "  Network: %s (cidr=%s)\n", n.Name, n.CIDR); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
	for _, inst := range cfg.Instances {
		if _, err := fmt.Fprintf(out, "  Instance: %s (mac=%s, network=%s)\n", inst.Name, inst.MAC, inst.Network); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}

	return nil
}
