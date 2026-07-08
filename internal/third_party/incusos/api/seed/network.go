// Vendored from github.com/lxc/incus-os @ 829415edc9ad3ff4242e209567ceef8f1f29ad87
// (incus-osd/api/seed/network.go), Apache-2.0 license — see third_party/incus-os/COPYING.
// Modified: import path rewritten to this module's vendored api package.
// Regenerate via scripts/vendor-incusos.sh; do not hand-edit beyond what
// that script does.

package seed

import (
	"github.com/ehharvey/homelab-ops/internal/third_party/incusos/api"
)

// Network represents the network seed.
type Network struct {
	api.SystemNetworkConfig `yaml:",inline"`

	Version string `json:"version" yaml:"version"`
}
