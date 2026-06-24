// Vendored from github.com/lxc/incus-os @ 10705332c6cf4eadf63be1b8db99d19f64bc0ca6
// (incus-osd/api/seed/incus.go), Apache-2.0 license — see third_party/incus-os/COPYING.
// Unmodified — this one genuinely needs the real github.com/lxc/incus/v7/shared/api types (Incus's own InitPreseed/CertificatesPost), so its import is left as upstream wrote it rather than rewritten to our vendored package.
// Regenerate via scripts/vendor-incusos.sh; do not hand-edit beyond what
// that script does.

package seed

import (
	incusapi "github.com/lxc/incus/v7/shared/api"
)

// Incus represents the Incus seed file.
type Incus struct {
	Version string `json:"version" yaml:"version"`

	ApplyDefaults bool                  `json:"apply_defaults" yaml:"apply_defaults"`
	Preseed       *incusapi.InitPreseed `json:"preseed"        yaml:"preseed"`
}
