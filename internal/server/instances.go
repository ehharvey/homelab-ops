package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

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

// handleInstanceImage regenerates and streams a bootable .img for name's
// instance on every request (no caching — docs/Decisions.md §3), reusing
// resolveInstanceSeed for the same 404/422 lookup as handleInstanceSeed, then
// seed.Write + the ImageBuilder (flasher.Run) into throwaway temp paths.
func handleInstanceImage(store Store, certs CertSource, builder ImageBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		if certs == nil {
			http.Error(w, "cert source not configured (set CLIENT_CERT_PATH)", http.StatusServiceUnavailable)
			return
		}
		if builder == nil {
			http.Error(w, "image generation not configured (set BASE_IMAGE_PATH)", http.StatusServiceUnavailable)
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

		// Regenerate into throwaway temp paths; the defers below reclaim both
		// regardless of how the handler returns, including a mid-stream client
		// disconnect. No concurrency limit — 0.x is single-user (docs/Out of
		// Scope.md), so N concurrent full-image copies is an accepted cost, not
		// an oversight.
		seedDir, err := os.MkdirTemp("", "seed-*")
		if err != nil {
			log.Printf("create temp seed dir for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(seedDir) //nolint:errcheck // best-effort cleanup of a temp dir

		if err := seed.Write(seedDir, bundle, true); err != nil {
			log.Printf("write seed for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		// CreateTemp only reserves a unique name here; flasher.Run re-creates
		// this path itself (it copies the base image over it), so close the
		// handle immediately.
		outFile, err := os.CreateTemp("", "image-*.img")
		if err != nil {
			log.Printf("create temp image for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		outPath := outFile.Name()
		_ = outFile.Close()
		defer os.Remove(outPath) //nolint:errcheck // best-effort cleanup of a temp file

		if err := builder.Build(r.Context(), seedDir, outPath, &flasherLogWriter{instance: name}); err != nil {
			log.Printf("build image for %q: %v", name, err)
			http.Error(w, "could not build image", http.StatusBadGateway)
			return
		}

		img, err := os.Open(outPath) //nolint:gosec // outPath is a server-created temp file, not client input
		if err != nil {
			log.Printf("open built image for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		defer img.Close() //nolint:errcheck // read-only file, nothing to flush

		info, err := img.Stat()
		if err != nil {
			log.Printf("stat built image for %q: %v", name, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".img"))
		// ServeContent streams straight from the *os.File (never buffering the
		// multi-GB image in memory), sets Content-Length from the stat above, and
		// honors Range requests so a download can resume over a flaky link.
		http.ServeContent(w, r, name+".img", info.ModTime(), img)
	}
}

// flasherLogWriter forwards the flasher-tool subprocess's output to the
// standard logger, one log line per write, prefixed with the instance name so
// concurrent builds stay distinguishable.
type flasherLogWriter struct{ instance string }

func (l *flasherLogWriter) Write(p []byte) (int, error) {
	log.Printf("flasher-tool %q: %q", l.instance, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// ensure flasherLogWriter satisfies io.Writer.
var _ io.Writer = (*flasherLogWriter)(nil)
