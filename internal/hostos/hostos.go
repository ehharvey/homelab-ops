// Package hostos is the host-OS seam (docs/Decisions.md §31, §15
// Prerequisite 1): one Backend per host operating system an Instance can run,
// chosen by its os field (config.Instance.HostOS). A Backend validates an
// Instance for its OS, renders the install-time config the machine boots with,
// and renders the install media that config goes into.
//
// IncusOS is the only backend. It wraps internal/seed and internal/flasher
// rather than moving them, so its output is byte-for-byte what those packages
// produced before the seam existed (pinned by TestIncusOSSeedGolden).
//
// There is no upgrade operation: IncusOS updates itself, so there is nothing
// to wrap. The interface gains one with the first backend that needs it
// (Talos, Phase 6), so that backend's real behaviour shapes its signature.
package hostos

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// Backend is one host OS's implementation of the seam.
type Backend interface {
	// OS is the config.Instance os value this backend serves.
	OS() string
	// ValidateInstance checks inst against what this OS's install config
	// can express. RenderConfig runs the same checks, so a caller that
	// renders needn't call it first.
	ValidateInstance(inst config.Instance) error
	// RenderConfig renders the files the machine's install media carries,
	// in the order they're written.
	RenderConfig(in ConfigInput) ([]File, error)
	// RenderImage bakes a directory of RenderConfig's files (as WriteFiles
	// wrote them) into install media at in.OutputImage.
	RenderImage(ctx context.Context, in ImageInput) error
}

// File is one rendered config file, named as the install media expects it.
type File struct {
	Name string
	Data []byte
}

// Tunnel is the WireGuard tunnel to the web app that a rendered config
// carries, plus the one-time bootstrap client cert the web app authenticates
// over it with (docs/Decisions.md §4, §23).
type Tunnel struct {
	AppPublicKey   wireguard.PublicKey
	AppEndpoint    string // host:port a node dials to reach the web app
	NodePrivateKey wireguard.PrivateKey
	// BootstrapCertPEM is trusted alongside ConfigInput.ClientCertPEM.
	BootstrapCertPEM []byte
}

// ConfigInput is everything RenderConfig needs for one Instance.
type ConfigInput struct {
	Network  config.Network
	Instance config.Instance
	// ClientCertPEM is the deployment's break-glass client cert (gen-cert's
	// client.crt), trusted on first boot.
	ClientCertPEM []byte
	// Tunnel is nil for a config with no tunnel: the bootstrap CLI's node
	// #0, rendered before any web app exists.
	Tunnel *Tunnel
	// ForceInstall and ForceReboot are IncusOS's install-seed flags
	// (render-seed's --force-install and --force-reboot).
	ForceInstall bool
	ForceReboot  bool
}

// ImageInput is everything RenderImage needs.
type ImageInput struct {
	// ConfigDir holds RenderConfig's files, as WriteFiles wrote them.
	ConfigDir string
	// BaseImage is the operator-supplied base image for this OS. It is
	// never modified.
	BaseImage string
	// OutputImage is where the install media is written.
	OutputImage string
	// Force allows overwriting an existing OutputImage.
	Force bool
	// ToolPath is the image tool binary; empty means the backend's default,
	// resolved from $PATH.
	ToolPath string
	// Stdout and Stderr receive the image tool's output.
	Stdout io.Writer
	Stderr io.Writer
}

// backends holds every registered Backend, keyed by its OS. A backend lands
// together with its config.HostOSImplemented entry; TestBackendsMatchConfig
// keeps the two in step.
var backends = map[string]Backend{
	config.OSIncusOS: IncusOS,
}

// IncusOS is the IncusOS backend. It's exported for the callers that can't
// dispatch on an Instance's os yet and so use it directly (build-image, and
// the web app's image builder); everything else goes through For.
var IncusOS Backend = incusOS{}

// For returns the Backend for hostOS, a config.Instance os value.
// config.Validate rejects an os with no backend, so this fails only for a
// config that skipped validation.
func For(hostOS string) (Backend, error) {
	b, ok := backends[hostOS]
	if !ok {
		return nil, fmt.Errorf("no host OS backend for %q", hostOS)
	}
	return b, nil
}

// ForInstance returns the Backend for inst's host OS (config.Instance.HostOS,
// which defaults an omitted os).
func ForInstance(inst config.Instance) (Backend, error) {
	return For(inst.HostOS())
}

// RenderConfig renders in.Instance's config with the backend its host OS
// selects.
func RenderConfig(in ConfigInput) ([]File, error) {
	b, err := ForInstance(in.Instance)
	if err != nil {
		return nil, fmt.Errorf("instance %q: %w", in.Instance.Name, err)
	}
	return b.RenderConfig(in)
}

// WriteFiles writes files into dir, creating it if needed. It refuses to
// overwrite an existing file unless force is true, and checks every file
// before writing any, so a refusal leaves dir as it found it rather than
// holding a mix of new and old files.
func WriteFiles(dir string, files []File, force bool) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	if !force {
		for _, f := range files {
			path := filepath.Join(dir, f.Name)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
		}
	}

	for _, f := range files {
		path := filepath.Join(dir, f.Name)
		if err := os.WriteFile(path, f.Data, 0o644); err != nil { //nolint:gosec // G306: seed YAML is not secret; the 0644 seed.Write always used, kept so output is unchanged
			return fmt.Errorf("write %s: %w", f.Name, err)
		}
	}

	return nil
}
