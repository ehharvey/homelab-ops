// Package cmd implements the bootstrap CLI's cobra commands.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/cert"
)

var (
	genCertOutputDir    string
	genCertCommonName   string
	genCertValidityDays int
	genCertForce        bool
)

var genCertCmd = &cobra.Command{
	Use:   "gen-cert",
	Short: "Generate a self-signed client cert/key pair offline",
	Long: `gen-cert generates a self-signed ECDSA P-384 client certificate and key
entirely offline, with no network dependency. The key becomes the credential
used later to authenticate directly against node #0's Incus API.`,
	RunE: runGenCert,
}

func init() {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "node0"
	}

	genCertCmd.Flags().StringVar(&genCertOutputDir, "output-dir", "./bootstrap-output/cert", "directory to write client.crt and client.key to")
	genCertCmd.Flags().StringVar(&genCertCommonName, "common-name", "bootstrap@"+hostname, "Subject CommonName for the generated certificate")
	genCertCmd.Flags().IntVar(&genCertValidityDays, "validity-days", 3650, "certificate validity period in days")
	genCertCmd.Flags().BoolVar(&genCertForce, "force", false, "overwrite an existing cert/key pair in --output-dir")

	rootCmd.AddCommand(genCertCmd)
}

func runGenCert(cmd *cobra.Command, _ []string) error {
	pair, err := cert.Generate(cert.Options{
		CommonName:   genCertCommonName,
		ValidityDays: genCertValidityDays,
	})
	if err != nil {
		return fmt.Errorf("generate cert: %w", err)
	}

	certPath, keyPath, err := cert.Write(genCertOutputDir, pair, genCertForce)
	if err != nil {
		return fmt.Errorf("write cert: %w", err)
	}

	fingerprint, err := cert.Fingerprint(pair.CertPEM)
	if err != nil {
		return fmt.Errorf("compute fingerprint: %w", err)
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Generated cert: %s\nGenerated key:  %s\nSHA-256 fingerprint: %s\n", certPath, keyPath, fingerprint); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}
