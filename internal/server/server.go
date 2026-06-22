// Package server provides the homelab-ops web app's HTTP server, decoupled
// from cmd/web's process wiring so it can be exercised directly in tests.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// Syncer pulls fleet config from its source and reports the resulting
// commit SHA. Implemented by internal/configsync.Syncer.
type Syncer interface {
	Sync(ctx context.Context) (config.Config, string, error)
}

// Store persists the synced Config snapshot so it survives across sync
// runs. Implemented by internal/store.Store.
type Store interface {
	Replace(ctx context.Context, cfg config.Config, commit string, now time.Time) error
	LastSync(ctx context.Context) (commit string, syncedAt time.Time, ok bool, err error)
	Networks(ctx context.Context) ([]config.Network, error)
	Instances(ctx context.Context) ([]config.Instance, error)
}

// New builds the web app's HTTP handler. syncer and store may be nil, in
// which case POST /sync and the read endpoints report themselves as
// unconfigured rather than failing /healthz.
func New(syncer Syncer, store Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("POST /sync", handleSync(syncer, store))
	mux.HandleFunc("GET /status", handleStatus(store))
	mux.HandleFunc("GET /networks", handleNetworks(store))
	mux.HandleFunc("GET /instances", handleInstances(store))
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

func handleSync(syncer Syncer, store Store) http.HandlerFunc {
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

		if store != nil {
			if err := store.Replace(r.Context(), cfg, sha, time.Now()); err != nil {
				log.Printf("store sync result: %v", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}

		writeJSON(w, syncResponse{
			Commit:    sha,
			Networks:  len(cfg.Networks),
			Instances: len(cfg.Instances),
		})
	}
}

type statusResponse struct {
	Synced   bool   `json:"synced"`
	Commit   string `json:"commit,omitempty"`
	SyncedAt string `json:"synced_at,omitempty"`
}

func handleStatus(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}

		commit, syncedAt, ok, err := store.LastSync(r.Context())
		if err != nil {
			log.Printf("query last sync: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !ok {
			writeJSON(w, statusResponse{Synced: false})
			return
		}
		writeJSON(w, statusResponse{Synced: true, Commit: commit, SyncedAt: syncedAt.Format(time.RFC3339)})
	}
}

func handleNetworks(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		networks, err := store.Networks(r.Context())
		if err != nil {
			log.Printf("query networks: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, networks)
	}
}

func handleInstances(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		instances, err := store.Instances(r.Context())
		if err != nil {
			log.Printf("query instances: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, instances)
	}
}

// writeJSON encodes v into a buffer first so a (very unlikely) encoding
// error can still produce a clean error response instead of a partially
// written body followed by a superfluous WriteHeader call.
func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}
