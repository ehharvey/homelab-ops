package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/secureboot"
)

var (
	genSBOutputDir     string
	genSBOfflineDir    string
	genSBNoMicrosoftCA bool
	genSBValidityDays  int
	genSBForce         bool
)

var genSecureBootKeysCmd = &cobra.Command{
	Use:   "gen-secureboot-keys",
	Short: "Generate the Secure Boot PK/KEK/db hierarchy and its enrollment payloads",
	Long: `gen-secureboot-keys generates the project's own UEFI Secure Boot key
hierarchy offline: an RSA-2048 Platform Key (PK), a Key Exchange Key (KEK)
issued by PK, and a signature database key (db) issued by KEK.

It also writes the enrollment payloads a machine in Setup Mode consumes on
first boot: an EFI signature list (.esl) and a signed authenticated variable
(.auth) for each of PK, KEK and db, and for dbx when db includes Microsoft's
CA. Copy the .auth files to /loader/keys/<name>/ on install media for
systemd-boot to auto-enroll.

db includes Microsoft's third-party UEFI CA certificates (2011 and 2023) by
default, because GPU, NIC and storage option ROMs are signed under them and a
machine without them may not boot. dbx then holds Microsoft's x64 list of
revoked binaries, re-signed with this KEK: that CA has signed shims and boot
loaders that were later revoked, and clearing a machine's factory keys can
empty its dbx. --no-microsoft-ca leaves out both, and with --force removes a
dbx.esl and dbx.auth an earlier run left in --output-dir. Microsoft's KEK and
Windows PCA are never enrolled.

The PK and KEK private keys are written to --offline-dir, apart from
everything else, so that directory can be moved to offline storage. The db
key stays in --output-dir: sign-efi uses it.`,
	RunE: runGenSecureBootKeys,
}

func init() {
	f := genSecureBootKeysCmd.Flags()
	f.StringVar(&genSBOutputDir, "output-dir", "./bootstrap-output/secureboot", "directory for the certs, db key, signature lists and .auth payloads")
	f.StringVar(&genSBOfflineDir, "offline-dir", "./bootstrap-output/secureboot-offline", "directory for the PK and KEK private keys, meant to go offline")
	f.BoolVar(&genSBNoMicrosoftCA, "no-microsoft-ca", false, "leave Microsoft's UEFI CA 2011/2023 certs out of db, and write no dbx (option ROMs signed under them will not run)")
	f.IntVar(&genSBValidityDays, "validity-days", 3650, "certificate validity period in days")
	f.BoolVar(&genSBForce, "force", false, "overwrite existing files in --output-dir and --offline-dir")

	rootCmd.AddCommand(genSecureBootKeysCmd)
}

func runGenSecureBootKeys(cmd *cobra.Command, _ []string) error {
	h, err := secureboot.Generate(secureboot.Options{ValidityDays: genSBValidityDays})
	if err != nil {
		return fmt.Errorf("generate keys: %w", err)
	}
	p, err := h.Payloads(secureboot.PayloadOptions{IncludeMicrosoftCA: !genSBNoMicrosoftCA})
	if err != nil {
		return fmt.Errorf("build enrollment payloads: %w", err)
	}
	paths, err := secureboot.Write(genSBOutputDir, genSBOfflineDir, h, p, genSBForce)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	out := cmd.OutOrStdout()
	for _, path := range paths {
		if _, err := fmt.Fprintf(out, "Wrote %s\n", path); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
	msCA, dbx := "included", "Microsoft's x64 revocations, re-signed by KEK"
	if genSBNoMicrosoftCA {
		msCA, dbx = "not included", "none written"
	}
	if _, err := fmt.Fprintf(out, "Owner GUID: %s\nMicrosoft UEFI CA in db: %s\ndbx: %s\nMove %s (the PK and KEK private keys) to offline storage.\n",
		h.OwnerGUID, msCA, dbx, genSBOfflineDir); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
