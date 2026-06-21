package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/flasher"
)

var (
	buildImageSeedDir        string
	buildImageBaseImage      string
	buildImageOutput         string
	buildImageForce          bool
	buildImageFlasherToolBin string
)

var buildImageCmd = &cobra.Command{
	Use:   "build-image",
	Short: "Inject a rendered seed into a base IncusOS image, producing a .img",
	Long: `build-image copies a base IncusOS raw image to --output and shells out to
the upstream flasher-tool binary (go install
github.com/lxc/incus-os/incus-osd/cmd/flasher-tool) to inject the seed
files from --seed-dir (the output of render-seed) into the copy in place.
The result is a .img ready to dd onto a USB stick.`,
	RunE: runBuildImage,
}

func init() {
	buildImageCmd.Flags().StringVar(&buildImageSeedDir, "seed-dir", "./bootstrap-output/seed", "directory containing install.yaml, network.yaml, and applications.yaml (render-seed's output)")
	buildImageCmd.Flags().StringVar(&buildImageBaseImage, "image", "", "path to a base IncusOS raw image (required)")
	buildImageCmd.Flags().StringVar(&buildImageOutput, "output", "./bootstrap-output/img/incusos.img", "path to write the seeded .img to")
	buildImageCmd.Flags().BoolVar(&buildImageForce, "force", false, "overwrite an existing file at --output")
	buildImageCmd.Flags().StringVar(&buildImageFlasherToolBin, "flasher-tool-bin", "flasher-tool", "flasher-tool binary to invoke (resolved via $PATH unless it contains a slash)")

	rootCmd.AddCommand(buildImageCmd)
}

func runBuildImage(cmd *cobra.Command, _ []string) error {
	if buildImageBaseImage == "" {
		return fmt.Errorf("--image is required")
	}

	err := flasher.Run(cmd.Context(), flasher.Options{
		SeedDir:     buildImageSeedDir,
		BaseImage:   buildImageBaseImage,
		OutputImage: buildImageOutput,
		Force:       buildImageForce,
		BinPath:     buildImageFlasherToolBin,
		Stdout:      cmd.OutOrStdout(),
		Stderr:      cmd.ErrOrStderr(),
	})
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Wrote %s — ready to dd onto a USB stick\n", buildImageOutput); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}
