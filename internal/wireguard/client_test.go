package wireguard

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadPrivateKeyFileAcceptsWgGenkeyOutput(t *testing.T) {
	priv, _, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	path := filepath.Join(t.TempDir(), "op.key")
	// `wg genkey > op.key` leaves a trailing newline.
	if err := os.WriteFile(path, []byte(priv.Base64()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPrivateKeyFile(path)
	if err != nil {
		t.Fatalf("ReadPrivateKeyFile: %v", err)
	}
	if got != priv {
		t.Error("ReadPrivateKeyFile returned a different key")
	}
}

func TestReadPrivateKeyFileRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPrivateKeyFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateKeyFile(short); err == nil {
		t.Error("a 3-byte key was accepted")
	}
}

func TestResolveEndpoints(t *testing.T) {
	ctx := context.Background()
	got, err := ResolveEndpoints(ctx, "192.0.2.7:51820")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddrPort("192.0.2.7:51820") {
		t.Errorf("IP literal: got %v, %v", got, err)
	}
	got, err = ResolveEndpoints(ctx, "localhost:51820")
	if err != nil || len(got) == 0 {
		t.Fatalf("localhost: got %v, %v", got, err)
	}
	for _, ep := range got {
		if !ep.Addr().IsLoopback() || ep.Port() != 51820 {
			t.Errorf("localhost: got %v; want only loopback addresses on 51820", got)
		}
	}
	for _, bad := range []string{"", "192.0.2.7", "host-without-port", "192.0.2.7:notaport"} {
		if _, err := ResolveEndpoints(ctx, bad); err == nil {
			t.Errorf("ResolveEndpoints(%q) succeeded", bad)
		}
	}
}

// TestStartClientReachesItsPeer is the operator CLI's join (#196): the far
// side lists the client without an endpoint, as the web app lists an
// operator peer, and the client's own endpoint for it is enough to connect.
func TestStartClientReachesItsPeer(t *testing.T) {
	srvPriv, srvPub, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Start(Options{PrivateKey: srvPriv, LocalAddr: WebAppAddr})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Close() //nolint:errcheck // test cleanup
	srvPort, err := srv.ListenPort()
	if err != nil {
		t.Fatal(err)
	}

	cliPriv, cliPub, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	cliAddr := netip.MustParseAddr("10.100.0.240")
	if err := srv.UpsertPeer(cliPub, cliAddr); err != nil {
		t.Fatal(err)
	}
	ln, err := srv.Listen(APIPort)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test-only server
		_, _ = io.WriteString(conn, conn.RemoteAddr().String())
	}()

	cli, err := StartClient(ClientOptions{
		PrivateKey:    cliPriv,
		LocalAddr:     cliAddr,
		PeerPublicKey: srvPub,
		PeerTunnelIP:  WebAppAddr,
		PeerEndpoint:  netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(srvPort)), //nolint:gosec // ListenPort returns a real bound UDP port
	})
	if err != nil {
		t.Fatalf("StartClient: %v", err)
	}
	defer cli.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := cli.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", WebAppAddr, APIPort))
	if err != nil {
		t.Fatalf("dial through the tunnel: %v", err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if want := cliAddr.String(); len(got) < len(want) || string(got[:len(want)]) != want {
		t.Errorf("server saw the client as %q, want %s:<port>", got, want)
	}
}
