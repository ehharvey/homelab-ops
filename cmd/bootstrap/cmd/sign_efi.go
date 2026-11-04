package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/secureboot"
)

var (
	signEFIKey    string
	signEFICert   string
	signEFIOutput string
	signEFIForce  bool
)

var signEFICmd = &cobra.Command{
	Use:   "sign-efi <image>",
	Short: "Authenticode-sign a PE/EFI binary (a UKI) with the Secure Boot db key",
	Long: `sign-efi Authenticode-signs a PE/EFI binary, such as a unified kernel image,
with the db key gen-secureboot-keys wrote, so firmware that enrolled that db
runs it. The signed image goes to --output (default <image>.signed); an image
that is already signed is refused.`,
	Args: cobra.ExactArgs(1),
	RunE: runSignEFI,
}

func init() {
	f := signEFICmd.Flags()
	f.StringVar(&signEFIKey, "key", "./bootstrap-output/secureboot/db.key", "PEM private key to sign with")
	f.StringVar(&signEFICert, "cert", "./bootstrap-output/secureboot/db.crt", "PEM certificate matching --key")
	f.StringVar(&signEFIOutput, "output", "", "where to write the signed image (default <image>.signed)")
	f.BoolVar(&signEFIForce, "force", false, "overwrite an existing --output")

	rootCmd.AddCommand(signEFICmd)
}

func runSignEFI(cmd *cobra.Command, args []string) error {
	input := args[0]
	output := signEFIOutput
	if output == "" {
		output = input + ".signed"
	}

	certPEM, err := os.ReadFile(signEFICert) //nolint:gosec // G304: operator-supplied --cert path
	if err != nil {
		return fmt.Errorf("read cert: %w", err)
	}
	keyPEM, err := os.ReadFile(signEFIKey) //nolint:gosec // G304: operator-supplied --key path
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	signer, err := secureboot.LoadKeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load %s and %s: %w", signEFICert, signEFIKey, err)
	}

	image, err := os.ReadFile(input) //nolint:gosec // G304: operator-supplied image path
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}
	signed, err := secureboot.SignPE(image, *signer)
	if err != nil {
		return fmt.Errorf("sign %s: %w", input, err)
	}

	// Written to a temporary file and renamed into place, so a failed write
	// leaves no truncated image that a later run would refuse to replace.
	if err := secureboot.WriteFiles([]secureboot.File{{Path: output, Data: signed}}, signEFIForce); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Signed %s with %q\nWrote %s\n", input, signer.Cert.Subject.CommonName, output); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
