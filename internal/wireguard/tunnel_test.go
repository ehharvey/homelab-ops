package wireguard

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestGenerateKeypairDerivesMatchingPublicKey(t *testing.T) {
	priv, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if pub != PublicKeyOf(priv) {
		t.Errorf("PublicKeyOf(priv) = %s, want %s (the pub GenerateKeypair returned)", PublicKeyOf(priv), pub)
	}
}

func TestGenerateKeypairIsClamped(t *testing.T) {
	priv, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if priv[0]&0b0000_0111 != 0 {
		t.Errorf("private key byte 0 = %08b, low 3 bits should be cleared (RFC7748 clamping)", priv[0])
	}
	if priv[31]&0b1000_0000 != 0 {
		t.Errorf("private key byte 31 = %08b, high bit should be cleared (RFC7748 clamping)", priv[31])
	}
	if priv[31]&0b0100_0000 == 0 {
		t.Errorf("private key byte 31 = %08b, second-highest bit should be set (RFC7748 clamping)", priv[31])
	}
}

func TestGenerateKeypairProducesDistinctKeys(t *testing.T) {
	priv1, pub1, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	priv2, pub2, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if priv1 == priv2 {
		t.Error("two GenerateKeypair calls produced the same private key")
	}
	if pub1 == pub2 {
		t.Error("two GenerateKeypair calls produced the same public key")
	}
}

func TestPublicKeyBase64RoundTrips(t *testing.T) {
	_, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if pub.String() != pub.Base64() {
		t.Errorf("PublicKey.String() = %q, want it to equal Base64() = %q", pub.String(), pub.Base64())
	}
	if len(pub.Base64()) == 0 {
		t.Error("PublicKey.Base64() is empty")
	}
}

func TestStartAndClose(t *testing.T) {
	priv, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	tun, err := Start(Options{PrivateKey: priv, ListenPort: 0, LocalAddr: WebAppAddr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Close() //nolint:errcheck // test cleanup

	if tun.PublicKey() != pub {
		t.Errorf("PublicKey() = %s, want %s", tun.PublicKey(), pub)
	}
}

func TestUpsertPeerAcceptsAPeer(t *testing.T) {
	priv, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	tun, err := Start(Options{PrivateKey: priv, ListenPort: 0, LocalAddr: WebAppAddr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Close() //nolint:errcheck // test cleanup

	_, peerPub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}

	if err := tun.UpsertPeer(peerPub, netip.MustParseAddr("10.100.0.5")); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	// Calling it again for the same peer (e.g. a re-sync) must not error —
	// this is the idempotency AssignTunnelIPs/reconcileTunnelPeers depend on.
	if err := tun.UpsertPeer(peerPub, netip.MustParseAddr("10.100.0.5")); err != nil {
		t.Fatalf("second UpsertPeer for the same peer: %v", err)
	}
}

func TestDialContextRejectsNonTCP(t *testing.T) {
	priv, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	tun, err := Start(Options{PrivateKey: priv, ListenPort: 0, LocalAddr: WebAppAddr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tun.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := tun.DialContext(ctx, "udp", "10.100.0.5:8443"); err == nil {
		t.Error("DialContext with network=udp = nil error, want an error (only tcp is supported)")
	}
}

// TestTunnelEndToEndHandshakeAndDial is the one test in this package that
// proves the actual WireGuard mechanism end to end — two independent
// Tunnels (userspace network stacks, real UDP sockets on loopback)
// completing a real Noise handshake and passing TCP traffic through it.
// Everything else in this package tests lifecycle/config-plumbing in
// isolation; this is the load-bearing proof that the plumbing actually
// works, standing in for a real node (which would need a live IncusOS
// boot to exercise the same path — see scripts/validate-issue-91.sh).
//
// Tunnel A plays the web app: passive, its peer entry for B carries no
// endpoint (exactly what UpsertPeer sends — the web app never knows a
// node's address in advance). Tunnel B plays a node: it knows A's
// endpoint up front (baked into a real node's seed via
// seed.WireGuard.AppEndpoint) and is the one that initiates. UpsertPeer
// deliberately has no way to set an endpoint (see its doc comment), so B's
// peer entry for A is built directly via the device's UAPI here, in the
// same shape internal/seed's rendered network.yaml causes IncusOS's own
// WireGuard implementation to configure on a real node.
func TestTunnelEndToEndHandshakeAndDial(t *testing.T) {
	privA, pubA, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	tunA, err := Start(Options{PrivateKey: privA, ListenPort: 0, LocalAddr: WebAppAddr})
	if err != nil {
		t.Fatalf("Start (A): %v", err)
	}
	defer tunA.Close() //nolint:errcheck // test cleanup

	portA, err := tunA.ListenPort()
	if err != nil {
		t.Fatalf("A.ListenPort: %v", err)
	}

	privB, pubB, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	nodeAddr := netip.MustParseAddr("10.100.0.5")
	tunB, err := Start(Options{PrivateKey: privB, ListenPort: 0, LocalAddr: nodeAddr})
	if err != nil {
		t.Fatalf("Start (B): %v", err)
	}
	defer tunB.Close() //nolint:errcheck // test cleanup

	if err := tunA.UpsertPeer(pubB, nodeAddr); err != nil {
		t.Fatalf("A.UpsertPeer(B): %v", err)
	}
	bUAPI := fmt.Sprintf(
		"public_key=%s\nendpoint=127.0.0.1:%d\nallowed_ip=%s/32\npersistent_keepalive_interval=1\n",
		hexKey(pubA), portA, WebAppAddr,
	)
	if err := tunB.dev.IpcSet(bUAPI); err != nil {
		t.Fatalf("B trust A (with endpoint): %v", err)
	}

	const marker = "hello through the tunnel"
	ln, err := tunA.net.ListenTCPAddrPort(netip.AddrPortFrom(WebAppAddr, 8443))
	if err != nil {
		t.Fatalf("A.net.ListenTCPAddrPort: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test-only echo server
		buf := make([]byte, len(marker))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		_, _ = conn.Write(buf)
	}()

	// B's PersistentKeepalive fires immediately on peer creation, but the
	// handshake still takes a moment — retry the dial rather than requiring
	// it to succeed first try.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var conn net.Conn
	deadline := time.Now().Add(20 * time.Second)
	var dialErr error
	for time.Now().Before(deadline) {
		conn, dialErr = tunB.DialContext(ctx, "tcp", fmt.Sprintf("%s:8443", WebAppAddr))
		if dialErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("B could not dial A through the tunnel within 20s: %v", dialErr)
	}
	defer conn.Close() //nolint:errcheck // test cleanup

	if _, err := conn.Write([]byte(marker)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, len(marker))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo through tunnel: %v", err)
	}
	if string(got) != marker {
		t.Errorf("echoed payload = %q, want %q", got, marker)
	}
}
