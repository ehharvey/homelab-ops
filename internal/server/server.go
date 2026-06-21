// Package server provides the homelab-ops web app's HTTP server, decoupled
// from cmd/web's process wiring so it can be exercised directly in tests.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// Syncer pulls fleet config from its source and reports the resulting
// commit SHA. Implemented by internal/configsync.Syncer.
type Syncer interface {
	Sync(ctx context.Context) (config.Config, string, error)
}

// New builds the web app's HTTP handler. syncer may be nil, in which case
// POST /sync reports itself as unconfigured rather than failing /healthz.
func New(syncer Syncer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("POST /sync", handleSync(syncer))
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type syncResponse struct {
	Commit    string `json:"commit"`
	Networks  int    `json:"networks"`
	Instances int    `json:"instances"`
}

func handleSync(syncer Syncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if syncer == nil {
			http.Error(w, "sync not configured", http.StatusServiceUnavailable)
			return
		}

		cfg, sha, err := syncer.Sync(r.Context())
		if err != nil {
			// Avoid returning raw internal errors to clients; keep the
			// detail in the server log instead.
			log.Printf("sync failed: %v", err)
			http.Error(w, "sync failed", http.StatusBadGateway)
			return
		}

		// Encode into a buffer first so a (very unlikely) encoding error
		// can still produce a clean error response instead of a partially
		// written body followed by a superfluous WriteHeader call.
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(syncResponse{
			Commit:    sha,
			Networks:  len(cfg.Networks),
			Instances: len(cfg.Instances),
		}); err != nil {
			log.Printf("encode sync response: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}
}
