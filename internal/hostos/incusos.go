package hostos

import (
	"context"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/flasher"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

// incusOS is the IncusOS backend. Every method delegates to internal/seed or
// internal/flasher unchanged; its errors are theirs, unwrapped, so callers'
// messages read as they did before the seam.
type incusOS struct{}

func (incusOS) OS() string { return config.OSIncusOS }

func (incusOS) ValidateInstance(inst config.Instance) error {
	return seed.ValidateInstance(inst)
}

// RenderConfig renders IncusOS's four-file install seed (seed.Render) and
// marshals it (seed.Marshal).
func (incusOS) RenderConfig(in ConfigInput) ([]File, error) {
	var wg *seed.WireGuard
	if in.Tunnel != nil {
		wg = &seed.WireGuard{
			AppPublicKey:     in.Tunnel.AppPublicKey,
			AppEndpoint:      in.Tunnel.AppEndpoint,
			NodePrivateKey:   in.Tunnel.NodePrivateKey,
			BootstrapCertPEM: in.Tunnel.BootstrapCertPEM,
		}
	}

	bundle, err := seed.Render(in.Network, in.Instance, in.ClientCertPEM, wg, seed.Options{
		ForceInstall: in.ForceInstall,
		ForceReboot:  in.ForceReboot,
	})
	if err != nil {
		return nil, err
	}

	seedFiles, err := seed.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(seedFiles))
	for i, f := range seedFiles {
		files[i] = File{Name: f.Name, Data: f.Data}
	}
	return files, nil
}

// RenderImage injects the seed in in.ConfigDir into a copy of the base
// IncusOS image with flasher-tool (flasher.Run, which builds the seed tar
// with flasher.BuildSeedTar).
func (incusOS) RenderImage(ctx context.Context, in ImageInput) error {
	return flasher.Run(ctx, flasher.Options{
		SeedDir:     in.ConfigDir,
		BaseImage:   in.BaseImage,
		OutputImage: in.OutputImage,
		Force:       in.Force,
		BinPath:     in.ToolPath,
		Stdout:      in.Stdout,
		Stderr:      in.Stderr,
	})
}
