package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/internal/seed"
)

// errInstanceNotFound marks a resolveInstanceSeed failure caused by an
// unknown instance name, distinguishing a 404 from the various 422 causes
// below via errors.Is.
var errInstanceNotFound = errors.New("instance not found")

// instanceSeedResponse holds the four rendered seed documents, one YAML
// string per field — matching seed.Write's per-field marshaling — so each
// file stays independently addressable (e.g. via curl | jq) rather than
// bundled into one multi-doc blob.
type instanceSeedResponse struct {
	InstallYAML      string `json:"install_yaml"`
	NetworkYAML      string `json:"network_yaml"`
	ApplicationsYAML string `json:"applications_yaml"`
	IncusYAML        string `json:"incus_yaml"`
}

func handleInstanceSeed(store Store, certs CertSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		if certs == nil {
			http.Error(w, "cert source not configured (set CLIENT_CERT_PATH)", http.StatusServiceUnavailable)
			return
		}

		name := r.PathValue("name")

		bundle, err := resolveInstanceSeed(r.Context(), store, certs, name)
		if err != nil {
			if errors.Is(err, errInstanceNotFound) {
				http.NotFound(w, r)
				return
			}
			log.Printf("resolve instance seed %q: %v", name, err)
			http.Error(w, "could not render seed", http.StatusUnprocessableEntity)
			return
		}

		installYAML, err := yaml.Marshal(bundle.Install)
		if err != nil {
			log.Printf("marshal install.yaml for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		networkYAML, err := yaml.Marshal(bundle.Network)
		if err != nil {
			log.Printf("marshal network.yaml for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		applicationsYAML, err := yaml.Marshal(bundle.Applications)
		if err != nil {
			log.Printf("marshal applications.yaml for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		incusYAML, err := yaml.Marshal(bundle.Incus)
		if err != nil {
			log.Printf("marshal incus.yaml for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		writeJSON(w, instanceSeedResponse{
			InstallYAML:      string(installYAML),
			NetworkYAML:      string(networkYAML),
			ApplicationsYAML: string(applicationsYAML),
			IncusYAML:        string(incusYAML),
		})
	}
}

// resolveInstanceSeed looks up name's Instance and its Network from store,
// fetches the deployment's break-glass cert from certs, and renders the
// seed bundle. Shared by handleInstanceSeed (which marshals the result to
// JSON) and #39's future image route (which will seed.Write it to a temp
// dir for flasher.Run instead).
func resolveInstanceSeed(ctx context.Context, store Store, certs CertSource, name string) (seed.Bundle, error) {
	inst, ok, err := store.Instance(ctx, name)
	if err != nil {
		return seed.Bundle{}, fmt.Errorf("query instance %q: %w", name, err)
	}
	if !ok {
		return seed.Bundle{}, fmt.Errorf("instance %q: %w", name, errInstanceNotFound)
	}

	net, ok, err := store.Network(ctx, inst.Network)
	if err != nil {
		return seed.Bundle{}, fmt.Errorf("query network %q: %w", inst.Network, err)
	}
	if !ok {
		// The instance points at a network absent from the same synced
		// snapshot — a data-integrity problem in the synced fleet, not a
		// bad request, so it's distinguished in the log from a Render
		// rejection below even though both map to the same client status.
		return seed.Bundle{}, fmt.Errorf("instance %q targets network %q, which is missing from the synced snapshot", name, inst.Network)
	}

	certPEM, err := certs.ClientCertPEM(ctx)
	if err != nil {
		return seed.Bundle{}, fmt.Errorf("read client cert: %w", err)
	}

	bundle, err := seed.Render(net, inst, certPEM, seed.Options{})
	if err != nil {
		return seed.Bundle{}, fmt.Errorf("render seed for instance %q: %w", name, err)
	}
	return bundle, nil
}
