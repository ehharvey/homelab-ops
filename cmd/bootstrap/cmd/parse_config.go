package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/config"
)

var parseConfigFile string

var parseConfigCmd = &cobra.Command{
	Use:   "parse-config",
	Short: "Parse a fleet definition YAML file and summarize what it contains",
	Long: `parse-config reads a k8s-style, multi-document YAML fleet definition
(one or more documents discriminated by "kind: Network" / "kind: Instance")
and prints a summary of the parsed Networks and Instances: for each Network,
its dhcp.mode and its effective static range (dhcp_excluded_range) and pool
(dhcp.pool), each marked written or derived, with the uncarved range shown
when the network, broadcast, gateway or an in-cidr dns address was carved out
of it. It performs no seed rendering — this only proves the file parses into
in-memory objects.`,
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

	var b strings.Builder
	fmt.Fprintf(&b, "Parsed %d Network(s), %d Instance(s):\n", len(cfg.Networks), len(cfg.Instances))
	for _, n := range cfg.Networks {
		writeNetworkSummary(&b, n)
	}
	for _, inst := range cfg.Instances {
		fmt.Fprintf(&b, "  Instance: %s (mac=%s, network=%s)\n", inst.Name, inst.MAC, inst.Network)
	}
	if _, err := io.WriteString(cmd.OutOrStdout(), b.String()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// writeNetworkSummary writes n's line and, when it has a cidr, its two
// ranges as config.Network.Allocation derives and carves them.
func writeNetworkSummary(w *strings.Builder, n config.Network) {
	if !n.CIDR.IsValid() {
		fmt.Fprintf(w, "  Network: %s (cidr=none, dhcp.mode=%s)\n", n.Name, n.DHCPMode())
		if n.Unaddressed() {
			fmt.Fprintf(w, "    no addressing: in a fleet the web app syncs, it comes from the addressing file\n")
		} else {
			// Partial: the web app rejects it rather than take the rest
			// from the addressing file (addressing.Resolve).
			fmt.Fprintf(w, "    partial addressing (%s, no cidr): set its cidr, or move all of it to the addressing file\n", strings.Join(n.AddressingFields(), ", "))
		}
		return
	}
	fmt.Fprintf(w, "  Network: %s (cidr=%s, dhcp.mode=%s)\n", n.Name, n.CIDR, n.DHCPMode())
	alloc, issues := n.Allocation()
	if !issues.Empty() {
		fmt.Fprintf(w, "    ranges not computed: %s\n", issues)
		return
	}
	fmt.Fprintf(w, "    static range: %s\n", describeSpan(alloc.Static))
	fmt.Fprintf(w, "    pool:         %s\n", describeSpan(alloc.Pool))
	// Allocation.Nodes' rule: the pool in full mode, the static range in any
	// other, where neither range written means no auto-assignment at all.
	switch {
	case alloc.Mode == config.DHCPFull:
		fmt.Fprintf(w, "    node addresses from the pool\n")
	case alloc.Static.Source == config.SpanUnset:
		fmt.Fprintf(w, "    node addresses: no auto-assignment range, so each instance needs a static_ip, anywhere in the cidr\n")
	default:
		fmt.Fprintf(w, "    node addresses from the static range\n")
	}
}

// describeSpan renders s's effective ranges and where they came from, plus
// its uncarved ranges when carving changed them.
func describeSpan(s config.Span) string {
	var src string
	switch s.Source {
	case config.SpanUnset:
		return "none (neither dhcp_excluded_range nor dhcp.pool is written)"
	case config.SpanWritten:
		src = "written as " + s.Field
	case config.SpanComplement:
		other := "dhcp.pool"
		if s.Field == other {
			other = "dhcp_excluded_range"
		}
		src = "derived: the cidr minus " + other
	case config.SpanWholeCIDR:
		src = "derived: the whole cidr"
	}
	out := config.FormatRanges(s.Effective) + " (" + src
	if s.Carved() {
		out += "; carved from " + config.FormatRanges(s.Uncarved)
	}
	return out + ")"
}
