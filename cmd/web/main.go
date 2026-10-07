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
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/flasher"
	"github.com/ehharvey/homelab-ops/internal/server"
	"github.com/ehharvey/homelab-ops/internal/store"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	if p := os.Getenv("PORT"); p != "" {
		//nolint:gosec // G706: p is an operator-supplied env var (not untrusted input) and %q quotes it
		log.Printf("PORT=%q is no longer read (#195): the API is served over the WireGuard tunnel, or on API_HOST_LISTEN_ADDR if set; /healthz is on HEALTH_LISTEN_ADDR", p)
	}

	syncer := newSyncer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, storePath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close() //nolint:errcheck // best-effort cleanup on shutdown

	operators, err := wireguard.ParseOperatorPeers(os.Getenv("WIREGUARD_OPERATOR_PEERS"))
	if err != nil {
		return fmt.Errorf("WIREGUARD_OPERATOR_PEERS: %w", err)
	}

	// Unlike the other optional sources (cert/image builder, which degrade to
	// "route reports itself unconfigured"), a *set* WIREGUARD_ENDPOINT
	// expresses explicit operator intent to run the tunnel — failing to
	// start it is fatal, not silently degraded. An unset WIREGUARD_ENDPOINT
	// leaves tunnelSrc as a true nil server.TunnelSource.
	var tunnelSrc server.TunnelSource
	var tun *wireguard.Tunnel
	if endpoint := os.Getenv("WIREGUARD_ENDPOINT"); endpoint != "" {
		ts, err := newTunnelSource(ctx, st, endpoint, operators)
		if err != nil {
			return fmt.Errorf("start wireguard tunnel: %w", err)
		}
		tunnelSrc, tun = ts, ts.tun
		defer tunnelSrc.Close() //nolint:errcheck // best-effort cleanup on shutdown
	} else if len(operators) > 0 {
		return errors.New("WIREGUARD_OPERATOR_PEERS is set but WIREGUARD_ENDPOINT is not: operator peers reach the API over the tunnel, which only runs when WIREGUARD_ENDPOINT is set")
	}

	// One Service shared between the HTTP handlers and the background poller
	// so their syncs serialize through a single lock.
	svc := server.NewService(syncer, st, newCertSource(), newImageBuilder(), tunnelSrc, st)

	listeners, err := listen(server.NewFromService(svc), tun, operators, os.Getenv("API_HOST_LISTEN_ADDR"))
	if err != nil {
		return err
	}

	if syncer != nil {
		if interval, ok := syncInterval(); ok {
			go pollSync(ctx, svc, interval)
		}
	}

	errCh := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() {
			if err := l.srv.Serve(l.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", l.name, err)
			}
		}()
	}

	var serveErr error
	select {
	case serveErr = <-errCh:
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, l := range listeners {
		if err := l.srv.Shutdown(shutdownCtx); err != nil && serveErr == nil {
			serveErr = fmt.Errorf("shut down %s: %w", l.name, err)
		}
	}
	return serveErr
}

// listener is one bound address and the server that serves it.
type listener struct {
	name string
	ln   net.Listener
	srv  *http.Server
}

// listen binds every listener the deployment config asks for (#195,
// docs/Decisions.md §28 and §29), before anything is served, so a bad or
// taken address fails startup instead of a background goroutine:
//
//   - health: GET /healthz only, on HEALTH_LISTEN_ADDR (default :8081),
//     whatever the API listener setting, for container health checks.
//   - tunnel (the default API listener): the API at wireguard.WebAppAddr,
//     port wireguard.APIPort, inside the tunnel, admitting operator peers
//     only. It runs whenever the tunnel does (tun non-nil).
//   - host (explicit opt-in): the API as plain HTTP on hostAddr
//     (API_HOST_LISTEN_ADDR), with no source filter. Off unless set; there
//     is no default address.
//
// A TLS host listener is the expected third mode (§29). It would be one
// more entry here; it is not built yet.
func listen(api http.Handler, tun *wireguard.Tunnel, operators []wireguard.OperatorPeer, hostAddr string) ([]listener, error) {
	var out []listener
	fail := func(err error) ([]listener, error) {
		for _, l := range out {
			_ = l.ln.Close()
		}
		return nil, err
	}
	add := func(name string, ln net.Listener, h http.Handler) {
		out = append(out, listener{name: name, ln: ln, srv: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}})
		//nolint:gosec // G706: the address is the bound listener's, from operator-supplied env vars (not untrusted input), and %q quotes it
		log.Printf("%s listening on %q", name, ln.Addr().String())
	}

	healthAddr := healthListenAddr()
	ln, err := net.Listen("tcp", healthAddr)
	if err != nil {
		return fail(fmt.Errorf("health listener on %s: %w", healthAddr, err))
	}
	add("health listener (/healthz only)", ln, server.NewHealthHandler())

	if tun != nil {
		ln, err := tun.Listen(wireguard.APIPort)
		if err != nil {
			return fail(fmt.Errorf("tunnel API listener: %w", err))
		}
		add(fmt.Sprintf("tunnel API listener (%d operator peers)", len(operators)), ln, server.OperatorsOnly(operators, api))
		if len(operators) == 0 {
			log.Print("WARNING: WIREGUARD_OPERATOR_PEERS is empty, so the tunnel API listener refuses every request; add an operator peer to use the API over the tunnel")
		}
	}

	if hostAddr != "" {
		ln, err := net.Listen("tcp", hostAddr)
		if err != nil {
			return fail(fmt.Errorf("API_HOST_LISTEN_ADDR %s: %w", hostAddr, err))
		}
		add("host API listener (plain HTTP, no source filter)", ln, api)
		//nolint:gosec // G706: hostAddr is an operator-supplied env var (not untrusted input) and %q quotes it
		log.Printf("WARNING: API_HOST_LISTEN_ADDR=%q exposes the whole API as plain HTTP with no authentication: anyone who can reach that address can call POST /instances/{name}/seed or GET /instances/{name}/image and receive that node's WireGuard private key. Bind it only to an address you trust (#195)", hostAddr)
	}

	if tun == nil && hostAddr == "" {
		log.Print("WARNING: the API is not served on any listener: set WIREGUARD_ENDPOINT and WIREGUARD_OPERATOR_PEERS (the default, tunnel-only posture), or opt in to API_HOST_LISTEN_ADDR")
	}
	return out, nil
}

// healthListenAddr reports the /healthz listener's address from
// HEALTH_LISTEN_ADDR, or ":8081" if unset. That listener serves nothing but
// /healthz, so binding every interface by default exposes no API route.
func healthListenAddr() string {
	if a := os.Getenv("HEALTH_LISTEN_ADDR"); a != "" {
		return a
	}
	return ":8081"
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

// newTunnelSource builds the web app's in-process WireGuard tunnel, bound
// to endpoint (the operator-supplied host:port nodes dial to reach this
// deployment — see run()'s WIREGUARD_ENDPOINT check, which only calls this
// when it's set), and registers operators as peers straight away, so the
// operator can reach the tunnel API before the first sync. Always returns
// either a usable source or a non-nil error — unlike newCertSource/
// newImageBuilder's nil-means-unconfigured convention, "unconfigured" is
// decided by the caller before this runs, since a *set* WIREGUARD_ENDPOINT
// that then fails to start is fatal (see run()'s comment). The identity
// persists in st (internal/wireguard.LoadOrGenerateIdentity), so it survives
// restarts without a new file-based deployment config surface.
// WIREGUARD_PORT overrides the UDP listen port (default 51820, WireGuard's
// IANA-assigned port).
func newTunnelSource(ctx context.Context, st *store.Store, endpoint string, operators []wireguard.OperatorPeer) (wireguardTunnelSource, error) {
	priv, err := wireguard.LoadOrGenerateIdentity(ctx, st)
	if err != nil {
		return wireguardTunnelSource{}, fmt.Errorf("load wireguard identity: %w", err)
	}
	tun, err := wireguard.Start(wireguard.Options{PrivateKey: priv, ListenPort: wireguardPort(), LocalAddr: wireguard.WebAppAddr})
	if err != nil {
		return wireguardTunnelSource{}, fmt.Errorf("start tunnel: %w", err)
	}
	for _, op := range operators {
		if err := tun.UpsertPeer(op.PublicKey, op.Addr); err != nil {
			_ = tun.Close()
			return wireguardTunnelSource{}, fmt.Errorf("register operator peer %s: %w", op.Addr, err)
		}
	}
	// Operators need this key for their own peer entry, and in the default
	// tunnel-only posture no API route can hand it to them. It is public.
	//nolint:gosec // G706: endpoint is an operator-supplied env var (not untrusted input) and %q quotes it
	log.Printf("wireguard public key %s, overlay address %s, endpoint %q", tun.PublicKey(), wireguard.WebAppAddr, endpoint)
	return wireguardTunnelSource{tun: tun, endpoint: endpoint, operators: operators}, nil
}

// wireguardPort reports the configured WireGuard UDP listen port from
// WIREGUARD_PORT, or 51820 (WireGuard's IANA-assigned default) if unset or
// invalid.
func wireguardPort() int {
	raw := os.Getenv("WIREGUARD_PORT")
	if raw == "" {
		return 51820
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		//nolint:gosec // G706: raw is an operator-supplied env var (not untrusted input) and %q quotes it
		log.Printf("invalid WIREGUARD_PORT %q, using default 51820: %v", raw, err)
		return 51820
	}
	return port
}

// wireguardTunnelSource implements server.TunnelSource over a live
// *wireguard.Tunnel plus the operator-supplied endpoint nodes dial to
// reach it (the tunnel itself has no way to learn its own externally
// reachable address, e.g. behind NAT), and the operator peers from
// WIREGUARD_OPERATOR_PEERS that every sync keeps registered (#195).
type wireguardTunnelSource struct {
	tun       *wireguard.Tunnel
	endpoint  string
	operators []wireguard.OperatorPeer
}

func (t wireguardTunnelSource) PublicKey() wireguard.PublicKey { return t.tun.PublicKey() }
func (t wireguardTunnelSource) Endpoint() string               { return t.endpoint }

func (t wireguardTunnelSource) OperatorPeers() []wireguard.OperatorPeer { return t.operators }

func (t wireguardTunnelSource) UpsertPeer(pub wireguard.PublicKey, tunnelIP netip.Addr) error {
	return t.tun.UpsertPeer(pub, tunnelIP)
}

func (t wireguardTunnelSource) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.tun.DialContext(ctx, network, address)
}

func (t wireguardTunnelSource) Close() error { return t.tun.Close() }
