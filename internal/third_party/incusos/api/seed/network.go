// Vendored from github.com/lxc/incus-os @ 95d29ace0fd42d0e42e69919ed44cc1eb3dd8191
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
