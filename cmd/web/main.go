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
	"sync"
	"syscall"
	"time"

	"github.com/ehharvey/homelab-ops/internal/addressing"
	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/deployconfig"
	"github.com/ehharvey/homelab-ops/internal/hostos"
	"github.com/ehharvey/homelab-ops/internal/server"
	"github.com/ehharvey/homelab-ops/internal/vault"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	// Every setting, read and validated up front (#241, absorbing #67): a bad
	// deployment stops here, naming each problem, before anything starts.
	cfg, err := deployconfig.Load()
	if err != nil {
		return err
	}
	if cfg.Dir != "" {
		//nolint:gosec // G706: the path is an operator-supplied env var (not untrusted input) and %q quotes it
		log.Printf("deployment config directory %q", cfg.Dir)
	}
	noteRetiredPort(cfg.RetiredPort)

	syncer := newSyncer(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The store starts locked, every time (#197, docs/Decisions.md §28):
	// nothing reads it until an operator supplies its key over the API.
	vlt, err := vault.New(cfg.StorePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer vlt.Close() //nolint:errcheck // best-effort cleanup on shutdown
	noteLockState(vlt)

	operators := cfg.OperatorPeers

	// Unlike the other optional sources (cert/image builder, which degrade to
	// "route reports itself unconfigured"), a *set* WIREGUARD_ENDPOINT
	// expresses explicit operator intent to run the tunnel — failing to
	// start it is fatal, not silently degraded. An unset WIREGUARD_ENDPOINT
	// leaves tunnelSrc as a true nil server.TunnelSource; deployconfig.Load
	// has already refused operator peers without one.
	var tunnelSrc server.TunnelSource
	var tun *wireguard.Tunnel
	if cfg.WireGuardEndpoint != "" {
		ts, err := newTunnelSource(cfg.WireGuardPrivateKey, cfg.WireGuardEndpoint, cfg.WireGuardPort, operators)
		if err != nil {
			return fmt.Errorf("start wireguard tunnel: %w", err)
		}
		tunnelSrc, tun = ts, ts.tun
		defer tunnelSrc.Close() //nolint:errcheck // best-effort cleanup on shutdown
	}

	// One Service shared between the HTTP handlers and the background poller
	// so their syncs serialize through a single lock.
	svc := server.NewService(syncer, vlt, newCertSource(cfg.ClientCertPath), newImageBuilder(cfg), tunnelSrc, vlt)
	// A deployed agent syncs from the same repo as this web app.
	svc.SetAgentRepo(agentdeploy.Repo{URL: cfg.ConfigRepoURL, Ref: cfg.ConfigRepoRef})
	addr := networkAddressing(cfg)
	if err := checkAddressing(ctx, addr); err != nil {
		return err
	}
	svc.SetAddressing(addr)

	// The operators first, while the store is locked, so they can reach the
	// key routes over the tunnel. Fatal, like starting the tunnel: nothing
	// could unlock the store otherwise.
	if err := svc.RegisterOperatorPeers(ctx); err != nil {
		return fmt.Errorf("register wireguard operator peers: %w", err)
	}

	// Every API listener serves the key routes, and the rest of the API only
	// once the store is unlocked. Nodes flashed before this restart must get
	// a handshake reply as soon as it is, not after the next sync (#269), so
	// unlocking restores the stored instances' peers, and only then lets the
	// poller start: a store is never locked again once unlocked, so that
	// happens once.
	unlocked := make(chan struct{})
	var unlockOnce sync.Once
	onUnlock := func(context.Context) error {
		defer unlockOnce.Do(func() { close(unlocked) })
		return restoreTunnelPeers(ctx, svc.RestoreTunnelPeers, restoreRetryFirst)
	}
	api := server.WithLock(vlt, server.NewFromService(svc), onUnlock)
	listeners, err := listen(api, tun, operators, cfg.HealthListenAddr, cfg.APIHostListenAddr)
	if err != nil {
		return err
	}

	if syncer != nil && cfg.SyncInterval > 0 {
		go pollSync(ctx, svc, cfg.SyncInterval, unlocked)
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

	if err := shutdown(listeners, 5*time.Second); err != nil && serveErr == nil {
		serveErr = err
	}
	return serveErr
}

// noteRetiredPort logs that a set PORT, which #195 retired, isn't read.
// deployconfig.Load has already refused one with no API listener configured:
// before #195 such a deployment served the API on PORT, and now it would serve
// it nowhere while /healthz, on its own listener, kept reporting healthy.
func noteRetiredPort(port string) {
	if port == "" {
		return
	}
	//nolint:gosec // G706: port is an operator-supplied env var (not untrusted input) and %q quotes it
	log.Printf("PORT=%q is no longer read (#195): the API is served over the WireGuard tunnel, or on API_HOST_LISTEN_ADDR if set; /healthz is on HEALTH_LISTEN_ADDR", port)
}

// shutdown drains every listener's server in parallel, all against one
// deadline. Shutdown closes a server's listener before it waits, so in
// parallel every listener stops accepting at once; one at a time, a slow
// drain on one (say, an image stream on the tunnel listener) would leave the
// later ones taking new requests that the deadline then cuts off. It reports
// the first listener's error, in listener order.
func shutdown(listeners []listener, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	errs := make([]error, len(listeners))
	var wg sync.WaitGroup
	for i, l := range listeners {
		wg.Go(func() {
			if err := l.srv.Shutdown(ctx); err != nil {
				errs[i] = fmt.Errorf("shut down %s: %w", l.name, err)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
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
//   - health: GET /healthz only, on healthAddr (HEALTH_LISTEN_ADDR, default :8081),
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
func listen(api http.Handler, tun *wireguard.Tunnel, operators []wireguard.OperatorPeer, healthAddr, hostAddr string) ([]listener, error) {
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

// newSyncer builds the config-sync client from the deployment config, or a nil
// server.Syncer if CONFIG_REPO_URL is unset (sync stays disabled). The return
// type is the interface, not *configsync.Syncer, so an unconfigured result is
// a true nil interface — boxing a nil *configsync.Syncer would defeat the
// nil-check in handleSync and panic on the first request.
func newSyncer(cfg deployconfig.Config) server.Syncer {
	if cfg.ConfigRepoURL == "" {
		return nil
	}
	return &configsync.Syncer{
		RepoURL: cfg.ConfigRepoURL,
		Ref:     cfg.ConfigRepoRef,
	}
}

// networkAddressing is where syncs take network addressing from
// (docs/Decisions.md §28, #199): CONFIG_REPO_PRIVATE=true allows addressing
// in git, and NETWORK_ADDRESSING_PATH names the addressing file for
// state-addressed networks. Both default off, so by default git addressing
// is rejected (§29's safe default: a public repo must not publish the LAN's
// addresses by accident). deployconfig.Load has already refused a
// CONFIG_REPO_PRIVATE other than "true" or "false".
func networkAddressing(cfg deployconfig.Config) server.Addressing {
	a := server.Addressing{RepoPrivate: cfg.ConfigRepoPrivate}
	if cfg.NetworkAddressingPath != "" {
		a.State = addressing.File{Path: cfg.NetworkAddressingPath}
	}
	return a
}

// checkAddressing reads the addressing file once at startup and runs the
// checks on its entries that need no git (addressing.CheckEntries: a name, a
// cidr, no name twice). So a file that doesn't parse, or fails them, stops
// the web app here, naming the setting, rather than failing every sync
// later. Syncs still re-read it each time, and check its entries against
// git. With neither source set, no network can be addressed, so every sync
// of a fleet that declares one is rejected; that's a warning, not fatal,
// since the fleet isn't known until the first sync.
func checkAddressing(ctx context.Context, a server.Addressing) error {
	if a.State != nil {
		nets, err := a.State.Networks(ctx)
		if err != nil {
			return fmt.Errorf("NETWORK_ADDRESSING_PATH: %w", err)
		}
		if issues := addressing.CheckEntries(nets); !issues.Empty() {
			return fmt.Errorf("NETWORK_ADDRESSING_PATH: %w", issues)
		}
	}
	if !a.RepoPrivate && a.State == nil {
		log.Printf("WARNING: neither CONFIG_REPO_PRIVATE=true nor NETWORK_ADDRESSING_PATH is set, so no network can be addressed: every sync of a fleet that declares a network will be rejected (docs/Decisions.md §28)")
	}
	return nil
}

// noteLockState logs the store's lock state at startup, and what unlocks it.
func noteLockState(v *vault.Vault) {
	if v.State() == vault.Locked {
		log.Print("store is locked: the API serves only /healthz, GET /store/state and the unlock and reset routes until `bootstrap api unlock` supplies its key")
		return
	}
	why := "STORE_PATH has no store file yet"
	if !v.Persistent() {
		why = "STORE_PATH is unset, so the store is in memory and every start begins empty"
	}
	log.Printf("store is not initialized (%s): the API serves only /healthz, GET /store/state and the init route until `bootstrap api init`", why)
}

// restoreRetryFirst and restoreRetryMax bound restoreTunnelPeers' retries:
// the first comes this long after the failure, and each later one waits
// twice as long, up to the max.
const (
	restoreRetryFirst = time.Second
	restoreRetryMax   = time.Minute
)

// restoreTunnelPeers runs restore (Service.RestoreTunnelPeers) under ctx,
// the process's, from WithLock's onUnlock. If it fails, it is retried in the
// background, after first and then backing off to restoreRetryMax, until it
// succeeds or ctx is done: nothing else re-registers the stored instances'
// peers before a sync, which with CONFIG_SYNC_INTERVAL unset waits on an
// operator's POST /sync (#269). The first failure is returned, for WithLock
// to log.
func restoreTunnelPeers(ctx context.Context, restore func(context.Context) error, first time.Duration) error {
	err := restore(ctx)
	if err == nil {
		return nil
	}
	go func() {
		wait := first
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			err := restore(ctx)
			if err == nil {
				log.Print("wireguard: stored peers restored on retry")
				return
			}
			wait = min(2*wait, restoreRetryMax)
			log.Printf("wireguard: restore stored peers: %v; retrying in %s", err, wait)
		}
	}()
	return fmt.Errorf("restore stored tunnel peers: %w; retrying in %s", err, first)
}

// pollSync syncs every interval once unlocked is closed, the first time as
// soon as it is. cmd/web closes it from WithLock's onUnlock, after the
// stored peers are restored.
func pollSync(ctx context.Context, svc *server.Service, interval time.Duration, unlocked <-chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case <-unlocked:
	}

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

// newCertSource builds the deployment's break-glass CertSource from
// CLIENT_CERT_PATH (or the config directory's client.crt), or a nil
// server.CertSource if unset (the seed route then reports itself
// unconfigured, mirroring newSyncer's nil-Syncer convention).
func newCertSource(path string) server.CertSource {
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
func newImageBuilder(cfg deployconfig.Config) server.ImageBuilder {
	if cfg.BaseImagePath == "" {
		return nil
	}
	return flasherBuilder{basePath: cfg.BaseImagePath, binPath: cfg.FlasherToolPath}
}

// flasherBuilder implements server.ImageBuilder with internal/hostos's IncusOS
// backend, which shells out to flasher-tool via internal/flasher.Run. Force is
// always set: the output path is a fresh per-request temp file the handler
// owns, so there is nothing to protect from overwrite.
type flasherBuilder struct{ basePath, binPath string }

func (b flasherBuilder) Build(ctx context.Context, hostOS, seedDir, outputPath string, logs io.Writer) error {
	// No dispatch on hostOS yet: a deployment supplies one base image and one
	// image tool (BASE_IMAGE_PATH, FLASHER_TOOL_PATH), and both are IncusOS's.
	// Building for another OS needs that OS's own base image and tool, which
	// arrive with its backend; until then any other OS is refused rather than
	// baked into an IncusOS image.
	if hostOS != config.OSIncusOS {
		return fmt.Errorf("no base image for host OS %q: BASE_IMAGE_PATH is an IncusOS image", hostOS)
	}
	return hostos.IncusOS.RenderImage(ctx, hostos.ImageInput{
		ConfigDir:   seedDir,
		BaseImage:   b.basePath,
		OutputImage: outputPath,
		Force:       true,
		ToolPath:    b.binPath,
		Stdout:      logs,
		Stderr:      logs,
	})
}

// newTunnelSource builds the web app's in-process WireGuard tunnel, bound
// to endpoint (the operator-supplied host:port nodes dial to reach this
// deployment — see run()'s WIREGUARD_ENDPOINT check, which only calls this
// when it's set). It registers no peers: run()'s RegisterOperatorPeers call
// adds the operators, so they reach the tunnel API's key routes while the
// store is locked, and unlocking it (RestoreTunnelPeers) adds the stored
// instances and then the operators again, in SyncOnce's order. Always returns
// either a usable source or a non-nil error — unlike newCertSource/
// newImageBuilder's nil-means-unconfigured convention, "unconfigured" is
// decided by the caller before this runs, since a *set* WIREGUARD_ENDPOINT
// that then fails to start is fatal (see run()'s comment). priv is the
// operator-supplied identity deployconfig.Load read from the deployment
// config directory's secrets/wireguard.key (#197, docs/Deployment.md), so
// the tunnel comes up while the store is still locked.
// port is the UDP listen port (WIREGUARD_PORT, default 51820, WireGuard's
// IANA-assigned port; 0 lets the OS pick).
func newTunnelSource(priv wireguard.PrivateKey, endpoint string, port int, operators []wireguard.OperatorPeer) (wireguardTunnelSource, error) {
	tun, err := wireguard.Start(wireguard.Options{PrivateKey: priv, ListenPort: port, LocalAddr: wireguard.WebAppAddr})
	if err != nil {
		return wireguardTunnelSource{}, fmt.Errorf("start tunnel: %w", err)
	}
	// Operators need this key for their own peer entry, and in the default
	// tunnel-only posture no API route can hand it to them. It is public.
	//nolint:gosec // G706: endpoint is an operator-supplied env var (not untrusted input) and %q quotes it
	log.Printf("wireguard public key %s, overlay address %s, endpoint %q", tun.PublicKey(), wireguard.WebAppAddr, endpoint)
	return wireguardTunnelSource{tun: tun, endpoint: endpoint, operators: operators}, nil
}

// wireguardTunnelSource implements server.TunnelSource over a live
// *wireguard.Tunnel plus the operator-supplied endpoint nodes dial to
// reach it (the tunnel itself has no way to learn its own externally
// reachable address, e.g. behind NAT), and the operator peers from
// WIREGUARD_OPERATOR_PEERS that startup and every sync register (#195, #269).
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
