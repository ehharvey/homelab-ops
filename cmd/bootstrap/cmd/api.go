package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ehharvey/homelab-ops/internal/apiclient"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// apiFlags are the api group's persistent flags: how this CLI joins the web
// app's WireGuard tunnel as an operator peer (#196, docs/Architecture.md
// § Who can reach the API). Flags rather than a config file, so a script
// (scripts/validate/) passes them inline; the private key is the one input
// read from a file, so it never shows in `ps`.
var apiFlags struct {
	privateKeyFile  string
	localAddr       string
	serverPublicKey string
	endpoint        string
	serverAddr      string
	listenPort      int
	connectTimeout  time.Duration
}

var apiCmd = &cobra.Command{
	Use:   "api",
	Short: "Call the web app's API over the operator's WireGuard tunnel",
	Long: `api calls the web app's HTTP API from this machine. It joins the web app's
WireGuard tunnel itself, as an in-process userspace peer, so it needs no OS
WireGuard interface and no root.

The key in --private-key-file must be listed in the web app's
WIREGUARD_OPERATOR_PEERS at --local-addr: being that peer is what
authenticates the call. The web app logs its own public key at startup
("wireguard public key ..."); pass it as --server-public-key, and the web
app's WIREGUARD_ENDPOINT as --endpoint.

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
	baseURL := "http://" + net.JoinHostPort(server.String(), strconv.Itoa(int(wireguard.APIPort)))
	return opts, endpoints, baseURL, nil
}

// apiConnect is how a verb gets its client and the cleanup that goes with it.
// Tests swap it for a client pointed at an httptest server.
var apiConnect = connectAPITunnel

// connectAPITunnel starts this CLI's WireGuard peer and returns a client that
// dials the API through it.
func connectAPITunnel(ctx context.Context) (*apiclient.Client, func(), error) {
	opts, endpoints, baseURL, err := apiTunnelOptions(ctx)
	if err != nil {
		return nil, nil, err
	}
	tun, dial, err := startFirstAnswering(ctx, opts, endpoints, apiFlags.connectTimeout)
	if err != nil {
		return nil, nil, err
	}
	closeTun := func() { tun.Close() } //nolint:errcheck,gosec // best-effort teardown on exit
	client, err := apiclient.New(baseURL, apiclient.HTTPClient(dial))
	if err != nil {
		closeTun()
		return nil, nil, err
	}
	return client, closeTun, nil
}

// startFirstAnswering starts the peer at each of endpoints in turn and dials
// the API through it once, waiting up to timeout for the handshake, until one
// answers. A name in --endpoint can resolve to several addresses (an A and an
// AAAA record) of which only some reach the web app, and a WireGuard peer has
// one endpoint, so only a handshake tells them apart. It returns that tunnel
// and the retrying dial to reach the API with. If none answers, the error
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
			conn.Close() //nolint:errcheck,gosec // a probe: the handshake is what it was for
			return tun, dial, nil
		}
		tun.Close() //nolint:errcheck,gosec // best-effort teardown of an endpoint that didn't answer
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("endpoint %s: %w", ep, err))
	}
	return nil, nil, errors.Join(errs...)
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
	return call(ctx, client)
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
