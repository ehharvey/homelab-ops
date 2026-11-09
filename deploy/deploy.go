// Package deploy holds the web app's production deployment files (#243), so
// the bootstrap CLI renders the appliance's seed from the same bytes the repo
// ships, and anyone can still read and run compose.yaml by hand. It is data
// only; internal/webappliance renders from it.
package deploy

import _ "embed"

// ComposeYAML is compose.yaml: the web app's production compose file, its
// image pinned by digest.
//
//go:embed compose.yaml
var ComposeYAML []byte
