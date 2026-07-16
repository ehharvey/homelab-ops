// Command validate-91-harness is test-only scaffolding for
// scripts/validate-issue-91.sh — it is NOT an operator-facing command like
// cmd/bootstrap or cmd/web, and ships no stability promise. It exists
// because the validate script needs to drive internal/wireguard's
// userspace tunnel and internal/nodeprovision's create-instance mechanism
// from outside a Go process, which isn't expressible from bash/curl: the
// tunnel terminates entirely in-process (a gVisor virtual network stack,
// no host TUN device), so nothing outside the process that owns it can
// dial through it.
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"
	"gopkg.in/yaml.v3"

	incusapi "github.com/ehharvey/homelab-ops/internal/third_party/incusos/api"
	incusseed "github.com/ehharvey/homelab-ops/internal/third_party/incusos/api/seed"

	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/store"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "validate-91-harness:", err)
		os.Exit(1)
	}
}

func run() error {
	mode := flag.String("mode", "", "genkey | probe | create-instance | extract-credential | patch-seed")
	privateKeyFile := flag.String("private-key-file", "", "path to this harness's own WireGuard private key (base64); genkey writes it, probe/create-instance read it")
	localAddr := flag.String("local-addr", "10.100.0.254", "this harness's own tunnel-overlay address — deliberately far from the normal instance-assignable range (internal/wireguard.OverlayCIDR, assigned from .2 up) to avoid colliding with a real instance's tunnel_ip, since the harness registers as a second, independent test peer on node0 alongside the real web app")
	peerPublicKey := flag.String("peer-public-key", "", "base64 WireGuard public key of the peer (node) to trust")
	peerTunnelIP := flag.String("peer-tunnel-ip", "", "the peer's tunnel-overlay address, e.g. 10.100.0.5")
	listenPort := flag.Int("listen-port", 0, "UDP port to bind (0 = OS picks a free port)")
	nodeAddr := flag.String("node-addr", "", "host:port of the node's Incus API to dial through the tunnel, e.g. 10.100.0.5:8443")
	timeout := flag.Duration("timeout", 60*time.Second, "how long to wait for the tunnel/dial/create to succeed")
	bootstrapCert := flag.String("bootstrap-cert", "", "path to the bootstrap client cert PEM (create-instance mode)")
	bootstrapKey := flag.String("bootstrap-key", "", "path to the bootstrap client key PEM (create-instance mode)")
	instanceName := flag.String("instance-name", "validate-91-placeholder", "name for the throwaway instance create-instance mode creates, or the instance whose credential to extract (extract-credential mode)")
	storagePool := flag.String("storage-pool", "default", "storage pool name to attach the throwaway instance's root disk to")
	storePath := flag.String("store-path", "", "path to a pulled copy of the web app's sqlite store (extract-credential mode)")
	outCert := flag.String("out-cert", "", "path to write the extracted bootstrap cert PEM to (extract-credential mode)")
	outKey := flag.String("out-key", "", "path to write the extracted bootstrap key PEM to (extract-credential mode)")
	networkYAMLPath := flag.String("network-yaml", "", "path to a rendered network.yaml to patch in place (patch-seed mode)")
	addRouteTo := flag.String("add-route-to", "", "CIDR to append a route for, via -add-route-via (patch-seed mode)")
	addRouteVia := flag.String("add-route-via", "", "gateway address for the route added by -add-route-to (patch-seed mode)")
	addPeerPublicKey := flag.String("add-peer-public-key", "", "base64 public key of a second WireGuard peer to append (patch-seed mode)")
	addPeerAllowedIP := flag.String("add-peer-allowed-ip", "", "CIDR (e.g. 10.100.0.254/32) allowed for the peer added by -add-peer-public-key (patch-seed mode)")
	flag.Parse()

	switch *mode {
	case "genkey":
		return runGenKey(*privateKeyFile)
	case "probe":
		return runProbe(*privateKeyFile, *localAddr, *peerPublicKey, *peerTunnelIP, *listenPort, *nodeAddr, *timeout)
	case "create-instance":
		return runCreateInstance(*privateKeyFile, *localAddr, *peerPublicKey, *peerTunnelIP, *listenPort, *nodeAddr, *timeout, *bootstrapCert, *bootstrapKey, *instanceName, *storagePool)
	case "extract-credential":
		return runExtractCredential(*storePath, *instanceName, *outCert, *outKey)
	case "patch-seed":
		return runPatchSeed(*networkYAMLPath, *addRouteTo, *addRouteVia, *addPeerPublicKey, *addPeerAllowedIP)
	default:
		return fmt.Errorf("unknown -mode %q (want genkey, probe, create-instance, extract-credential, or patch-seed)", *mode)
	}
}

// runPatchSeed appends a validate-script-only static route and a second
// WireGuard peer onto an already-rendered network.yaml — additions the
// real seed.Render never produces itself (see this repo's
// docs/Roadmap.md #91 plan for why these stay test-only rather than
// becoming production Render features). Uses the exact vendored types
// internal/seed renders with (internal/third_party/incusos), so the
// patched file stays byte-compatible with what IncusOS actually parses,
// and the same gopkg.in/yaml.v3 library the rest of this repo already
// depends on — no new environment dependency (e.g. Python + PyYAML) for
// the validate script to require.
func runPatchSeed(networkYAMLPath, routeTo, routeVia, peerPublicKeyB64, peerAllowedIP string) error {
	if networkYAMLPath == "" {
		return fmt.Errorf("-network-yaml is required")
	}

	raw, err := os.ReadFile(networkYAMLPath) //nolint:gosec // path is operator-supplied via --network-yaml, not untrusted input
	if err != nil {
		return fmt.Errorf("read network.yaml: %w", err)
	}
	var doc incusseed.Network
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse network.yaml: %w", err)
	}
	if len(doc.Interfaces) == 0 {
		return fmt.Errorf("network.yaml has no interfaces to attach a route to")
	}
	if len(doc.Wireguard) == 0 {
		return fmt.Errorf("network.yaml has no wireguard interface to attach a peer to")
	}

	if routeTo != "" || routeVia != "" {
		if routeTo == "" || routeVia == "" {
			return fmt.Errorf("-add-route-to and -add-route-via must be set together")
		}
		doc.Interfaces[0].Routes = append(doc.Interfaces[0].Routes, incusapi.SystemNetworkRoute{To: routeTo, Via: routeVia})
	}

	if peerPublicKeyB64 != "" || peerAllowedIP != "" {
		if peerPublicKeyB64 == "" || peerAllowedIP == "" {
			return fmt.Errorf("-add-peer-public-key and -add-peer-allowed-ip must be set together")
		}
		doc.Wireguard[0].Peers = append(doc.Wireguard[0].Peers, incusapi.SystemNetworkWireguardPeer{
			PublicKey:  peerPublicKeyB64,
			AllowedIPs: []string{peerAllowedIP},
		})
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal patched network.yaml: %w", err)
	}
	if err := os.WriteFile(networkYAMLPath, out, 0o644); err != nil { //nolint:gosec // G306: seed YAML is not secret, matching internal/seed.Write's existing convention
		return fmt.Errorf("write patched network.yaml: %w", err)
	}
	return nil
}

// runExtractCredential reads name's nodeprovision.Credential straight out of
// a pulled copy of the web app's sqlite store and writes its bootstrap
// cert/key PEM to outCertPath/outKeyPath. This is deliberately an offline
// file read, not a new HTTP route: internal/nodeprovision.Credential is
// secret material that must never become reachable through the web app's
// running API (see docs/Roadmap.md #91's security invariant) — reading the
// store file directly, the same way an operator with real filesystem
// access to STORE_PATH already could, doesn't add a new attack surface the
// way a route would.
func runExtractCredential(storePath, name, outCertPath, outKeyPath string) error {
	if storePath == "" || outCertPath == "" || outKeyPath == "" {
		return fmt.Errorf("-store-path, -out-cert, and -out-key are all required")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, storePath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close() //nolint:errcheck // read-only use, nothing to flush

	cred, ok, err := st.InstanceCredential(ctx, name)
	if err != nil {
		return fmt.Errorf("read credential for %q: %w", name, err)
	}
	if !ok {
		return fmt.Errorf("no credential minted for %q yet", name)
	}
	if err := os.WriteFile(outCertPath, cred.BootstrapCertPEM, 0o600); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(outKeyPath, cred.BootstrapKeyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

// runGenKey generates a fresh WireGuard keypair, writes the private half to
// privateKeyFile (base64, so scripts/validate-issue-91.sh can embed it as
// this harness's own persistent identity across separate invocations), and
// prints the public half to stdout so the caller can embed it into node0's
// test seed as a second trusted peer.
func runGenKey(privateKeyFile string) error {
	if privateKeyFile == "" {
		return fmt.Errorf("-private-key-file is required")
	}
	priv, pub, err := wireguard.GenerateKeypair()
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}
	if err := os.WriteFile(privateKeyFile, []byte(priv.Base64()), 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	fmt.Println(pub.Base64())
	return nil
}

// buildTunnel loads privateKeyFile, starts a Tunnel on listenPort, and
// registers peerPublicKeyB64/peerTunnelIPStr as a trusted peer — the
// common setup probe and create-instance both need.
func buildTunnel(privateKeyFile, localAddrStr, peerPublicKeyB64, peerTunnelIPStr string, listenPort int) (*wireguard.Tunnel, error) {
	if privateKeyFile == "" || peerPublicKeyB64 == "" || peerTunnelIPStr == "" {
		return nil, fmt.Errorf("-private-key-file, -peer-public-key, and -peer-tunnel-ip are all required")
	}

	privRaw, err := os.ReadFile(privateKeyFile) //nolint:gosec // path is operator-supplied via --private-key-file, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	priv, err := decodeKey(string(privRaw))
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}

	localAddr, err := netip.ParseAddr(localAddrStr)
	if err != nil {
		return nil, fmt.Errorf("parse -local-addr: %w", err)
	}
	peerPub, err := decodeKey(peerPublicKeyB64)
	if err != nil {
		return nil, fmt.Errorf("decode -peer-public-key: %w", err)
	}
	peerTunnelIP, err := netip.ParseAddr(peerTunnelIPStr)
	if err != nil {
		return nil, fmt.Errorf("parse -peer-tunnel-ip: %w", err)
	}

	tun, err := wireguard.Start(wireguard.Options{PrivateKey: priv, ListenPort: listenPort, LocalAddr: localAddr})
	if err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}
	if err := tun.UpsertPeer(wireguard.PublicKey(peerPub), peerTunnelIP); err != nil {
		tun.Close() //nolint:errcheck,gosec // best-effort cleanup on a failed setup; we're already returning the real error
		return nil, fmt.Errorf("register peer: %w", err)
	}
	return tun, nil
}

func decodeKey(b64 string) ([32]byte, error) {
	var key [32]byte
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return key, err
	}
	if len(raw) != 32 {
		return key, fmt.Errorf("key has length %d, want 32", len(raw))
	}
	copy(key[:], raw)
	return key, nil
}

// runProbe brings the tunnel up and dials nodeAddr over it, retrying until
// timeout — proving the WireGuard handshake completes and the node's Incus
// API is reachable through the tunnel specifically (see
// scripts/validate-issue-91.sh for why dialing the tunnel-overlay address
// is a meaningful proof even when the two ends share an L2 segment).
func runProbe(privateKeyFile, localAddr, peerPublicKey, peerTunnelIP string, listenPort int, nodeAddr string, timeout time.Duration) error {
	if nodeAddr == "" {
		return fmt.Errorf("-node-addr is required")
	}
	tun, err := buildTunnel(privateKeyFile, localAddr, peerPublicKey, peerTunnelIP, listenPort)
	if err != nil {
		return err
	}
	defer tun.Close() //nolint:errcheck // best-effort cleanup on exit

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := &http.Client{Transport: &http.Transport{
		DialContext: tun.DialContext,
		// Probing an Incus node's self-signed API with no CA to
		// chain-verify against — same trust model as
		// validate-issue-5.sh's `curl -k` probe and
		// internal/nodeprovision's identical choice (see its longer
		// comment for the full reasoning: trust comes from the WireGuard
		// tunnel + client cert, not server-cert verification). This
		// binary is test-only scaffolding for scripts/validate-issue-91.sh
		// (see package doc) — never built into the production web app. The
		// CodeQL alert this triggers is dismissed manually in GitHub's UI —
		// see internal/nodeprovision/provision.go's identical comment for
		// why inline suppression comments didn't work.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: matches this repo's existing direct-to-Incus trust model
	}}

	var lastErr error
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+nodeAddr+"/1.0", nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close() //nolint:errcheck,gosec // read-only response, nothing to flush
			fmt.Printf("reachable over the tunnel: %s\n", body)
			return nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("node never became reachable over the tunnel within %s: %w", timeout, lastErr)
}

// runCreateInstance brings the tunnel up and exercises
// nodeprovision.CreateInstance against a throwaway placeholder instance —
// proving the temp-cert-mint -> dial-over-tunnel -> create -> revoke
// mechanism #92's bootstrap deploy-agent will reuse for the real
// app-manager agent.
func runCreateInstance(privateKeyFile, localAddr, peerPublicKey, peerTunnelIP string, listenPort int, nodeAddr string, timeout time.Duration, bootstrapCertPath, bootstrapKeyPath, instanceName, storagePool string) error {
	if nodeAddr == "" || bootstrapCertPath == "" || bootstrapKeyPath == "" {
		return fmt.Errorf("-node-addr, -bootstrap-cert, and -bootstrap-key are all required")
	}
	tun, err := buildTunnel(privateKeyFile, localAddr, peerPublicKey, peerTunnelIP, listenPort)
	if err != nil {
		return err
	}
	defer tun.Close() //nolint:errcheck // best-effort cleanup on exit

	certPEM, err := os.ReadFile(bootstrapCertPath) //nolint:gosec // path is operator-supplied via --bootstrap-cert, not untrusted input
	if err != nil {
		return fmt.Errorf("read bootstrap cert: %w", err)
	}
	keyPEM, err := os.ReadFile(bootstrapKeyPath) //nolint:gosec // path is operator-supplied via --bootstrap-key, not untrusted input
	if err != nil {
		return fmt.Errorf("read bootstrap key: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req := lxcapi.InstancesPost{
		Name: instanceName,
		Type: lxcapi.InstanceTypeContainer,
		InstancePut: lxcapi.InstancePut{
			Devices: map[string]map[string]string{
				"root": {"type": "disk", "pool": storagePool, "path": "/"},
			},
		},
		Source: lxcapi.InstanceSource{Type: "none"},
	}

	if err := nodeprovision.CreateInstance(ctx, tun.DialContext, nodeAddr, certPEM, keyPEM, req); err != nil {
		return fmt.Errorf("create instance: %w", err)
	}
	fmt.Printf("created placeholder instance %q via one-time bootstrap credential (now revoked)\n", instanceName)
	return nil
}
