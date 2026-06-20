// Vendored from github.com/lxc/incus-os @ 3367fe0fbec5e4f163ad571691ef16b5e0a67705
// (incus-osd/api/seed/applications.go), Apache-2.0 license — see third_party/incus-os/COPYING.
// Unmodified.
// Regenerate via scripts/vendor-incusos.sh; do not hand-edit beyond what
// that script does.

package seed

// Applications represents the applications seed file.
type Applications struct {
	Version string `json:"version" yaml:"version"`

	Applications []Application `json:"applications" yaml:"applications"`
}

// Application represents a single application with the applications seed.
type Application struct {
	Name string `json:"name" yaml:"name"`
}
