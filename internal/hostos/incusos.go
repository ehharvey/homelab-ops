package hostos

import (
	"context"
	"io"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/flasher"
	"github.com/ehharvey/homelab-ops/internal/iso9660"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

// IncusOSSeedVolumeID is the label incus-osd looks for on separate seed
// media: a GPT partition or a filesystem labelled SEED_DATA, which it tries
// before the install image's own seed-data partition (upstream
// incus-osd/internal/seed/seed.go, getSeedPath).
const IncusOSSeedVolumeID = "SEED_DATA"

// incusOS is the IncusOS backend. Every method delegates to internal/seed,
// internal/flasher or internal/iso9660 unchanged; its errors are theirs, unwrapped, so callers'
// messages read as they did before the seam.
type incusOS struct{}

// IncusOS can read its install config from separate seed media.
var _ SeedMediaRenderer = incusOS{}

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
		TempDir:     in.TempDir,
		Stdout:      in.Stdout,
		Stderr:      in.Stderr,
	})
}

// RenderSeedMedia writes files as an ISO 9660 image labelled SEED_DATA, with
// the files at its root. incus-osd mounts such a volume read-only, as vfat
// and then as iso9660, and reads install, network, applications and incus
// (.yaml, .yml or .json) from its root. One image serves both a USB stick
// (dd) and a VM's CD-ROM.
//
// The volume is plain ISO 9660, so a file's name may use only letters,
// digits and "_", with at most one "." (iso9660.File.Name): every file
// RenderConfig writes today does, and
// TestIncusOSRenderSeedMediaIsSeedDataISO fails if one stops. A seed file
// incus-osd reads under a name with a "-" (operations-center.yaml,
// migration-manager.yaml) needs the media to carry long names first: Rock
// Ridge, or a FAT volume, which incus-osd tries ahead of iso9660.
//
// It still carries install.yaml after the install, so the media must come
// out before the installed system boots: incus-osd stops a boot that finds
// install media on an installed system.
func (incusOS) RenderSeedMedia(files []File, w io.Writer) error {
	isoFiles := make([]iso9660.File, len(files))
	for i, f := range files {
		isoFiles[i] = iso9660.File{Name: f.Name, Data: f.Data}
	}
	return iso9660.Write(w, iso9660.Options{VolumeID: IncusOSSeedVolumeID}, isoFiles)
}
