package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/webtls"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// apiFlags are the api group's persistent flags: how this CLI joins the web
// app's WireGuard tunnel as an operator peer (#196, docs/Architecture.md
// § Who can reach the API). Flags rather than a config file, so a script
// (scripts/validate/) passes them inline; the private key is the one input
// read from a file, so it never shows in `ps`. Over the tunnel the API is
// TLS (#397): the operator cert's two halves, and how the web app's TLS key
// is pinned, are flags too.
var apiFlags struct {
	privateKeyFile    string
	localAddr         string
	serverPublicKey   string
	endpoint          string
	serverAddr        string
	listenPort        int
	connectTimeout    time.Duration
	clientCert        string
	clientKey         string
	serverFingerprint string
	pinsFile          string
	repin             bool
}

// How the first-use pin confirmation reaches the operator. Tests swap them.
var (
	apiPromptIn  io.Reader = os.Stdin
	apiPromptOut io.Writer = os.Stderr
	// apiCanPrompt reports whether someone can see the prompt and answer
	// it: stdin, and stderr, where the prompt is written, are both
	// terminals. With stderr redirected the prompt would go unseen while
	// the run waits on stdin.
	apiCanPrompt = func() bool {
		return isTerminal(os.Stdin) && isTerminal(os.Stderr)
	}
)

// isTerminal reports whether f is a character device, as a terminal is.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var apiCmd = &cobra.Command{
	Use:   "api",
	Short: "Call the web app's API over the operator's WireGuard tunnel",
	Long: `api calls the web app's HTTP API from this machine. It joins the web app's
WireGuard tunnel itself, as an in-process userspace peer, so it needs no OS
WireGuard interface and no root.

The key in --private-key-file must be listed in the web app's
WIREGUARD_OPERATOR_PEERS at --local-addr: being that peer is the network
gate. The web app logs its own public key at startup
("wireguard public key ..."); pass it as --server-public-key, and the web
app's WIREGUARD_ENDPOINT as --endpoint.

The API inside the tunnel is TLS, and on top of that gate the web app
requires a listed operator cert (#397): pass the break-glass cert from
bootstrap gen-cert as --client-cert and --client-key. The web app's own cert
is self-signed, so it is pinned by its key's fingerprint, which the web app
logs at startup ("TLS server key fingerprint sha256//..."). On the first
connection the CLI shows the fingerprint and asks to confirm it, then
records it in --pins-file; a later change is refused. If the key changed on
purpose, --repin asks again and replaces the recorded pin. Without a
terminal (stdin and stderr) it never asks: pass the expected value as
--server-fingerprint, which is checked instead, with no pins file read or
written. A cert the web app doesn't list gets 403 on every request.

JSON routes print the server's answer on stdout. An error from the API
exits non-zero with the server's message.`,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		// Flags parsed: from here a failure is the API's or the tunnel's,
		// which a usage dump would only bury.
		cmd.SilenceUsage = true
	},
}

func init() {
	pf := apiCmd.PersistentFlags()
	pf.StringVar(&apiFlags.privateKeyFile, "private-key-file", "", "file holding this operator peer's base64 WireGuard private key, as `wg genkey` prints it (required)")
	pf.StringVar(&apiFlags.localAddr, "local-addr", "", "this peer's tunnel address, as listed for its key in the web app's WIREGUARD_OPERATOR_PEERS, in "+wireguard.OperatorCIDR.String()+" (required)")
	pf.StringVar(&apiFlags.serverPublicKey, "server-public-key", "", "the web app's base64 WireGuard public key, from its startup log (required)")
	pf.StringVar(&apiFlags.endpoint, "endpoint", "", "host:port of the web app's WireGuard UDP listener, its WIREGUARD_ENDPOINT; a name's addresses are tried in turn (required)")
	pf.StringVar(&apiFlags.serverAddr, "server-addr", wireguard.WebAppAddr.String(), "the web app's address inside the tunnel; the API is served there on port "+strconv.Itoa(int(wireguard.APIPort)))
	pf.IntVar(&apiFlags.listenPort, "listen-port", 0, "local UDP port for this peer's WireGuard socket (0: the OS picks one)")
	pf.DurationVar(&apiFlags.connectTimeout, "connect-timeout", 30*time.Second, "how long to wait for the tunnel to answer, at each address --endpoint resolves to, before giving up; bounds connecting only, not a download")
	pf.StringVar(&apiFlags.clientCert, "client-cert", "", "the operator cert presented to the web app (PEM), a cert its OPERATOR_CERTS_PATH or CLIENT_CERT_PATH lists, such as the client.crt bootstrap gen-cert writes; with --client-key")
	pf.StringVar(&apiFlags.clientKey, "client-key", "", "the private key for --client-cert (PEM), such as the client.key bootstrap gen-cert writes")
	pf.StringVar(&apiFlags.serverFingerprint, "server-fingerprint", "", "the web app's TLS key fingerprint (sha256//<base64>), from its startup log; checked instead of a recorded pin, and nothing is asked or recorded (for scripts; not with --pins-file or --repin)")
	pf.StringVar(&apiFlags.pinsFile, "pins-file", "", "where confirmed web app TLS key fingerprints are recorded, one per --server-public-key (default: homelab-ops/api-pins in the user config directory)")
	pf.BoolVar(&apiFlags.repin, "repin", false, "ignore the pin recorded for this web app, show its current TLS key fingerprint and ask to confirm it, and on yes replace the recorded pin (after its key changed on purpose; needs a terminal)")

	rootCmd.AddCommand(apiCmd)
}

// apiTunnelOptions checks the api group's flags and turns them into the
// tunnel to start, every address --endpoint resolves to (the options'
// PeerEndpoint is left for startFirstAnswering to set) and the API's base URL
// inside the tunnel.
func apiTunnelOptions(ctx context.Context) (wireguard.ClientOptions, []netip.AddrPort, string, error) {
	var opts wireguard.ClientOptions
	var missing []string
	for _, f := range []struct{ name, val string }{
		{"--private-key-file", apiFlags.privateKeyFile},
		{"--local-addr", apiFlags.localAddr},
		{"--server-public-key", apiFlags.serverPublicKey},
		{"--endpoint", apiFlags.endpoint},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return opts, nil, "", fmt.Errorf("missing required flags: %v", missing)
	}

	priv, err := wireguard.ReadPrivateKeyFile(apiFlags.privateKeyFile)
	if err != nil {
		return opts, nil, "", fmt.Errorf("--private-key-file: %w", err)
	}
	local, err := netip.ParseAddr(apiFlags.localAddr)
	if err != nil {
		return opts, nil, "", fmt.Errorf("--local-addr: %w", err)
	}
	serverPub, err := wireguard.ParsePublicKey(apiFlags.serverPublicKey)
	if err != nil {
		return opts, nil, "", fmt.Errorf("--server-public-key: %w", err)
	}
	server, err := netip.ParseAddr(apiFlags.serverAddr)
	if err != nil {
		return opts, nil, "", fmt.Errorf("--server-addr: %w", err)
	}
	if apiFlags.listenPort < 0 || apiFlags.listenPort > 65535 {
		return opts, nil, "", fmt.Errorf("--listen-port %d: want 0-65535", apiFlags.listenPort)
	}
	if apiFlags.connectTimeout <= 0 {
		return opts, nil, "", fmt.Errorf("--connect-timeout %s: want a positive duration", apiFlags.connectTimeout)
	}
	endpoints, err := wireguard.ResolveEndpoints(ctx, apiFlags.endpoint)
	if err != nil {
		return opts, nil, "", fmt.Errorf("--endpoint: %w", err)
	}

	opts = wireguard.ClientOptions{
		PrivateKey:    priv,
		LocalAddr:     local,
		ListenPort:    apiFlags.listenPort,
		PeerPublicKey: serverPub,
		PeerTunnelIP:  server,
	}
	baseURL := "https://" + net.JoinHostPort(server.String(), strconv.Itoa(int(wireguard.APIPort)))
	return opts, endpoints, baseURL, nil
}

// apiTLS is the TLS half of the api group's flags, checked before the
// tunnel starts.
type apiTLS struct {
	clientCert *tls.Certificate // nil: none presented
	expected   string           // --server-fingerprint
	pinsFile   string
	repin      bool
}

// apiTLSOptions checks --client-cert, --client-key, --server-fingerprint,
// --pins-file and --repin.
func apiTLSOptions() (apiTLS, error) {
	var out apiTLS
	pair, err := apiclient.LoadClientCert(apiFlags.clientCert, apiFlags.clientKey)
	if err != nil {
		return out, fmt.Errorf("--client-cert/--client-key: %w", err)
	}
	out.clientCert = pair
	if apiFlags.serverFingerprint != "" {
		fp, err := webtls.ParseFingerprint(apiFlags.serverFingerprint)
		if err != nil {
			return out, fmt.Errorf("--server-fingerprint: %w", err)
		}
		out.expected = fp
	}
	if apiFlags.repin && out.expected != "" {
		return out, errors.New("--repin and --server-fingerprint don't go together: --repin asks and records a new pin, --server-fingerprint is checked and records nothing")
	}
	if apiFlags.pinsFile != "" && out.expected != "" {
		return out, errors.New("--pins-file and --server-fingerprint don't go together: --server-fingerprint is checked instead of a recorded pin, so no pins file is read or written")
	}
	out.repin = apiFlags.repin
	out.pinsFile = apiFlags.pinsFile
	if out.pinsFile == "" && out.expected == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return out, fmt.Errorf("--pins-file: no default: %w", err)
		}
		out.pinsFile = filepath.Join(dir, "homelab-ops", "api-pins")
	}
	return out, nil
}

// apiConnect is how a verb gets its client and the cleanup that goes with it.
// Tests swap it for a client pointed at an httptest server.
var apiConnect = connectAPITunnel

// connectAPITunnel starts this CLI's WireGuard peer, pins the web app's TLS
// key (apiclient.Pinning: --server-fingerprint, else the recorded pin, else
// the operator's confirmation, which --repin asks for again), and returns a
// client that dials the API through the tunnel over TLS, presenting the
// operator cert. Only a first use or --repin reads the key with a probe of
// its own; otherwise the request's own handshake checks the pin.
func connectAPITunnel(ctx context.Context) (*apiclient.Client, func(), error) {
	opts, endpoints, baseURL, err := apiTunnelOptions(ctx)
	if err != nil {
		return nil, nil, err
	}
	tlsOpts, err := apiTLSOptions()
	if err != nil {
		return nil, nil, err
	}
	tun, dial, err := startFirstAnswering(ctx, opts, endpoints, apiFlags.connectTimeout)
	if err != nil {
		return nil, nil, err
	}
	closeTun := func() { tun.Close() } //nolint:errcheck,gosec // best-effort teardown on exit
	fail := func(err error) (*apiclient.Client, func(), error) {
		closeTun()
		return nil, nil, err
	}

	api := net.JoinHostPort(opts.PeerTunnelIP.String(), strconv.Itoa(int(wireguard.APIPort)))
	pinning := apiclient.Pinning{
		Expected: tlsOpts.expected,
		File:     tlsOpts.pinsFile,
		Server:   opts.PeerPublicKey.Base64(),
		Repin:    tlsOpts.repin,
		Probe: func(ctx context.Context) (string, error) {
			return apiclient.ServerFingerprint(ctx, dial, api)
		},
	}
	if tlsOpts.expected == "" && apiCanPrompt() {
		pinning.Confirm = apiclient.PromptConfirm(apiPromptIn, apiPromptOut, tlsOpts.pinsFile)
	}
	pin, source, err := pinning.Resolve(ctx)
	if err != nil {
		return fail(err)
	}
	httpClient, err := apiclient.PinnedHTTPClient(dial, apiclient.TLSOptions{ClientCert: tlsOpts.clientCert, ServerPin: pin, PinSource: source})
	if err != nil {
		return fail(err)
	}
	client, err := apiclient.New(baseURL, httpClient)
	if err != nil {
		return fail(err)
	}
	return client, closeTun, nil
}

// explainCertRefusal adds what to do to the web app's 403 for this CLI's
// client cert (webtls.RequireOperator's), which states only the refusal:
// presented says whether --client-cert was given. Any other error, a 403
// from the operator-peer filter included, is returned as is.
func explainCertRefusal(err error, presented bool) error {
	var apiErr *apiclient.APIError
	switch {
	case !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Message != webtls.NotOperatorMessage:
		return err
	case presented:
		return fmt.Errorf("%w (the web app doesn't list --client-cert: add it to its OPERATOR_CERTS_PATH, or make it its CLIENT_CERT_PATH)", err)
	default:
		return fmt.Errorf("%w (no client cert was presented: pass the operator cert with --client-cert and --client-key)", err)
	}
}

// startFirstAnswering starts the peer at each of endpoints in turn and dials
// the API through it once, waiting up to timeout for the handshake, until one
// answers. A name in --endpoint can resolve to several addresses (an A and an
// AAAA record) of which only some reach the web app, and a WireGuard peer has
// one endpoint, so only a handshake tells them apart. It returns that tunnel
// and the retrying dial to reach the API with, whose first call hands back
// the connection that answered instead of it being closed unused, which the
// web app would log as a failed TLS handshake. If none answers, the error
// names each address with why it failed.
func startFirstAnswering(ctx context.Context, opts wireguard.ClientOptions, endpoints []netip.AddrPort, timeout time.Duration) (*wireguard.Tunnel, apiclient.DialFunc, error) {
	api := net.JoinHostPort(opts.PeerTunnelIP.String(), strconv.Itoa(int(wireguard.APIPort)))
	var errs []error
	for _, ep := range endpoints {
		opts.PeerEndpoint = ep
		tun, err := wireguard.StartClient(opts)
		if err != nil {
			return nil, nil, err
		}
		dial := apiclient.RetryDial(tun.DialContext, timeout, time.Second)
		conn, err := dial(ctx, "tcp", api)
		if err == nil {
			return tun, reuseFirst(conn, api, dial), nil
		}
		tun.Close() //nolint:errcheck,gosec // best-effort teardown of an endpoint that didn't answer
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("endpoint %s: %w", ep, err))
	}
	return nil, nil, errors.Join(errs...)
}

// reuseFirst returns dial, except that its first call for address returns
// conn, already open to it.
func reuseFirst(conn net.Conn, address string, dial apiclient.DialFunc) apiclient.DialFunc {
	var mu sync.Mutex
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		c := conn
		if addr == address {
			conn = nil
		} else {
			c = nil
		}
		mu.Unlock()
		if c != nil {
			return c, nil
		}
		return dial(ctx, network, addr)
	}
}

// withAPI runs call with a connected client, under a context an interrupt
// cancels, so an aborted download still cleans up its partial file.
func withAPI(cmd *cobra.Command, call func(ctx context.Context, c *apiclient.Client) error) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, closeFn, err := apiConnect(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	return explainCertRefusal(call(ctx, client), apiFlags.clientCert != "")
}

// runAPIJSON is a JSON verb's whole body: call the route, print its answer.
func runAPIJSON(cmd *cobra.Command, route func(ctx context.Context, c *apiclient.Client) ([]byte, error)) error {
	return withAPI(cmd, func(ctx context.Context, c *apiclient.Client) error {
		body, err := route(ctx, c)
		if err != nil {
			return err
		}
		return writeWithNewline(cmd.OutOrStdout(), body)
	})
}

func writeWithNewline(w io.Writer, body []byte) error {
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// errNoOutput is a seed or image verb run without --output. There is no
// default: both carry the node's secrets, so where they land is the
// operator's explicit choice.
var errNoOutput = errors.New("--output is required (a file path, or - for stdout)")

// checkOutput fails fast on an --output that can't be written, before any
// request: the web app builds an image per request, which can take minutes,
// so an unwritable path must not wait for it. An existing path must open for
// writing, and is left as it is (writeOutput truncates it only once the
// answer arrives, so a failed request keeps an older download); a new one's
// directory must take a file, which is removed again.
func checkOutput(path string) error {
	if path == "" {
		return errNoOutput
	}
	if path == "-" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: path is operator-supplied via --output
	if errors.Is(err, fs.ErrNotExist) {
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path is operator-supplied via --output
		if err == nil {
			defer os.Remove(path) //nolint:errcheck // the probe file; writeOutput creates the real one
		}
	}
	if err != nil {
		return fmt.Errorf("open --output: %w", err)
	}
	return f.Close()
}

// writeOutput opens path for writing ("-" is stdout) and hands it to write.
// A new file is created 0600: seeds and images carry the node's secrets.
// When write fails, a regular file is removed rather than left looking like
// a complete download; a device (an image written straight to install
// media) is left alone.
func writeOutput(cmd *cobra.Command, path string, write func(w io.Writer) error) error {
	if path == "" {
		return errNoOutput
	}
	if path == "-" {
		return write(cmd.OutOrStdout())
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: path is operator-supplied via --output
	if err != nil {
		return fmt.Errorf("open --output: %w", err)
	}
	werr := write(f)
	cerr := f.Close()
	if werr == nil && cerr != nil {
		werr = fmt.Errorf("close %s: %w", path, cerr)
	}
	if werr != nil {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			_ = os.Remove(path)
		}
		return werr
	}
	return nil
}
