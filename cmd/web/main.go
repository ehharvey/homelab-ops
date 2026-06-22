// Command web is the homelab-ops web app: the always-on service that, per
// docs/Roadmap.md Phase 1 onward, syncs fleet config from GitHub and drives
// per-instance provisioning.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/server"
	"github.com/ehharvey/homelab-ops/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	addr := ":" + port()

	syncer := newSyncer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, storePath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close() //nolint:errcheck // best-effort cleanup on shutdown

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.New(syncer, st),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if syncer != nil {
		if interval, ok := syncInterval(); ok {
			go pollSync(ctx, syncer, st, interval)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// newSyncer builds the config-sync client from the environment, or nil if
// CONFIG_REPO_URL is unset (sync stays disabled).
func newSyncer() *configsync.Syncer {
	repoURL := os.Getenv("CONFIG_REPO_URL")
	if repoURL == "" {
		return nil
	}
	return &configsync.Syncer{
		RepoURL: repoURL,
		Ref:     os.Getenv("CONFIG_REPO_REF"),
	}
}

// syncInterval reports the configured background poll interval from
// CONFIG_SYNC_INTERVAL (e.g. "5m"), or ok=false if unset/invalid.
func syncInterval() (time.Duration, bool) {
	raw := os.Getenv("CONFIG_SYNC_INTERVAL")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("invalid CONFIG_SYNC_INTERVAL %q: %v", raw, err) //nolint:gosec // raw is operator-supplied via the CONFIG_SYNC_INTERVAL env var, not untrusted external input
		return 0, false
	}
	return d, true
}

func pollSync(ctx context.Context, syncer *configsync.Syncer, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg, sha, err := syncer.Sync(ctx)
			if err != nil {
				log.Printf("sync failed: %v", err)
				continue
			}
			if err := st.Replace(ctx, cfg, sha, time.Now()); err != nil {
				log.Printf("store sync result: %v", err)
				continue
			}
			log.Printf("synced commit %s: %d networks, %d instances", sha, len(cfg.Networks), len(cfg.Instances))
		}
	}
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

// storePath reports the local store's location from STORE_PATH, or
// ":memory:" (non-persistent, scoped to this process) if unset.
func storePath() string {
	if p := os.Getenv("STORE_PATH"); p != "" {
		return p
	}
	return ":memory:"
}
