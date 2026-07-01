// Command web is the homelab-ops web app: the always-on service that, per
// docs/Roadmap.md Phase 1 onward, syncs fleet config from GitHub and drives
// per-instance provisioning.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/flasher"
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

	// One Service shared between the HTTP handler and the background poller so
	// their syncs serialize through a single lock.
	svc := server.NewService(syncer, st, newCertSource(), newImageBuilder())

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewFromService(svc),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if syncer != nil {
		if interval, ok := syncInterval(); ok {
			go pollSync(ctx, svc, interval)
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

// newSyncer builds the config-sync client from the environment, or a nil
// server.Syncer if CONFIG_REPO_URL is unset (sync stays disabled). The return
// type is the interface, not *configsync.Syncer, so an unconfigured result is
// a true nil interface — boxing a nil *configsync.Syncer would defeat the
// nil-check in handleSync and panic on the first request.
func newSyncer() server.Syncer {
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
		//nolint:gosec // G706: raw is an operator-supplied env var (not untrusted input) and %q quotes it
		log.Printf("invalid CONFIG_SYNC_INTERVAL %q: %v", raw, err)
		return 0, false
	}
	return d, true
}

func pollSync(ctx context.Context, svc *server.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Sync once up front so a fresh process serves current state before
		// the first interval elapses, then again on every tick. Going through
		// svc.Sync (the same path POST /sync drives) surfaces diff warnings
		// against the prior snapshot rather than silently replacing it.
		if res, err := svc.Sync(ctx, time.Now()); err != nil {
			log.Printf("background sync: %v", err)
		} else {
			log.Printf("synced commit %s: %d networks, %d instances", res.Commit, len(res.Config.Networks), len(res.Config.Instances))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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

// newCertSource builds the deployment's break-glass CertSource from
// CLIENT_CERT_PATH, or a nil server.CertSource if unset (the seed route
// then reports itself unconfigured, mirroring newSyncer's nil-Syncer
// convention).
func newCertSource() server.CertSource {
	path := os.Getenv("CLIENT_CERT_PATH")
	if path == "" {
		return nil
	}
	return fileCertSource{path: path}
}

// fileCertSource reads the operator-supplied break-glass client cert from a
// local path on every call. It is never generated, minted, or persisted by
// the app — see docs/Architecture.md's "Cert sourcing".
type fileCertSource struct{ path string }

func (f fileCertSource) ClientCertPEM(_ context.Context) ([]byte, error) {
	return os.ReadFile(f.path) //nolint:gosec // path is operator-supplied deployment config, not untrusted input
}

// newImageBuilder builds the flasher-backed ImageBuilder from BASE_IMAGE_PATH
// (the operator-supplied base IncusOS raw image), or a nil server.ImageBuilder
// if unset (the image route then reports itself unconfigured, mirroring
// newCertSource's nil-CertSource convention). FLASHER_TOOL_PATH overrides the
// flasher-tool binary location — the Docker image sets it to /flasher-tool
// since distroless has no $PATH; unset falls back to resolving "flasher-tool"
// from $PATH for dev/devcontainer use.
func newImageBuilder() server.ImageBuilder {
	base := os.Getenv("BASE_IMAGE_PATH")
	if base == "" {
		return nil
	}
	return flasherBuilder{basePath: base, binPath: os.Getenv("FLASHER_TOOL_PATH")}
}

// flasherBuilder implements server.ImageBuilder by shelling out to flasher-tool
// via internal/flasher.Run. Force is always set: the output path is a fresh
// per-request temp file the handler owns, so there is nothing to protect from
// overwrite.
type flasherBuilder struct{ basePath, binPath string }

func (b flasherBuilder) Build(ctx context.Context, seedDir, outputPath string, logs io.Writer) error {
	return flasher.Run(ctx, flasher.Options{
		SeedDir:     seedDir,
		BaseImage:   b.basePath,
		OutputImage: outputPath,
		Force:       true,
		BinPath:     b.binPath,
		Stdout:      logs,
		Stderr:      logs,
	})
}
