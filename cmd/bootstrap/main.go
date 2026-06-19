// Command bootstrap is the offline CLI for provisioning node #0.
package main

import (
	"os"

	"github.com/ehharvey/homelab-ops/cmd/bootstrap/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
