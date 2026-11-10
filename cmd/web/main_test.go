package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/server"
	"github.com/ehharvey/homelab-ops/internal/store"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

type countingSyncer struct{ n int64 }

func (c *countingSyncer) Sync(context.Context) (config.Config, string, error) {
	atomic.AddInt64(&c.n, 1)
	return config.Config{}, "sha", nil
}

func TestPollSyncRunsAtStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	cs := &countingSyncer{}
	svc := server.NewService(cs, st, nil, nil, nil, nil)

	// A long interval means only the startup sync can fire within the test
	// window, so observing one sync proves pollSync doesn't wait for the
	// first tick to do its initial sync.
	done, unlocked := make(chan struct{}), make(chan struct{})
	close(unlocked)
	go func() {
		pollSync(ctx, svc, time.Hour, unlocked)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&cs.n) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt64(&cs.n) == 0 {
		t.Fatal("pollSync did not sync at startup before the first tick")
	}

	cancel()
	<-done
}

// TestPollSyncWaitsForUnlock pins that the poller never syncs a locked store
// (#197), and syncs as soon as it is unlocked rather than a tick later.
func TestPollSyncWaitsForUnlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	cs := &countingSyncer{}
	svc := server.NewService(cs, st, nil, nil, nil, nil)
	done, unlocked := make(chan struct{}), make(chan struct{})
	go func() {
		pollSync(ctx, svc, time.Hour, unlocked)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt64(&cs.n); n != 0 {
		t.Fatalf("pollSync synced %d times while the store was locked, want 0", n)
	}
	close(unlocked)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&cs.n) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt64(&cs.n) == 0 {
		t.Fatal("pollSync did not sync once the store was unlocked")
	}

	cancel()
	<-done
}

// A peer restore that fails on unlock is retried until it succeeds, since
// nothing else registers the stored peers before a sync (#269).
func TestRestoreTunnelPeersRetriesAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int64
	restored := make(chan struct{})
	restore := func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("upsert operator peer: transient")
		}
		close(restored)
		return nil
	}
	if err := restoreTunnelPeers(ctx, restore, time.Millisecond); err == nil {
		t.Fatal("restoreTunnelPeers = nil, want the first failure")
	}
	select {
	case <-restored:
	case <-time.After(2 * time.Second):
		t.Fatalf("the restore wasn't retried to success: %d calls", calls.Load())
	}
	if err := restoreTunnelPeers(ctx, func(context.Context) error { return nil }, time.Millisecond); err != nil {
		t.Errorf("restoreTunnelPeers with a working restore = %v", err)
	}
}

func TestFileCertSourceReadsConfiguredPath(t *testing.T) {
	pair, err := cert.Generate(cert.Options{CommonName: "test", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}

	path := filepath.Join(t.TempDir(), "client.crt")
	if err := os.WriteFile(path, pair.CertPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	got, err := (fileCertSource{path: path}).ClientCertPEM(context.Background())
	if err != nil {
		t.Fatalf("ClientCertPEM: %v", err)
	}
	if string(got) != string(pair.CertPEM) {
		t.Errorf("ClientCertPEM returned %q, want %q", got, pair.CertPEM)
	}
}

func TestFileCertSourceMissingFileReturnsClearError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.crt")

	_, err := (fileCertSource{path: path}).ClientCertPEM(context.Background())
	if err == nil {
		t.Fatal("ClientCertPEM with a missing file = nil error, want non-nil")
	}
}

func TestNewCertSourceUnsetReturnsNil(t *testing.T) {
	if cs := newCertSource(""); cs != nil {
		t.Errorf("newCertSource with CLIENT_CERT_PATH unset = %v, want nil", cs)
	}
}

// TestNewTunnelSourceUsesSuppliedIdentity pins that the tunnel's identity is
// the operator-supplied key (#197), not one generated or kept in the store:
// the same key gives the same public key every node's seed already trusts.
func TestNewTunnelSourceUsesSuppliedIdentity(t *testing.T) {
	priv, pub, err := wireguard.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	ts, err := newTunnelSource(priv, "203.0.113.1:51820", 0, nil)
	if err != nil {
		t.Fatalf("newTunnelSource: %v", err)
	}
	if ts.tun == nil {
		t.Fatal("newTunnelSource returned no tunnel")
	}
	defer ts.Close() //nolint:errcheck // test cleanup

	if ts.Endpoint() != "203.0.113.1:51820" {
		t.Errorf("Endpoint() = %q, want %q", ts.Endpoint(), "203.0.113.1:51820")
	}
	if ts.PublicKey() != pub {
		t.Errorf("PublicKey() = %s, want the supplied key's %s", ts.PublicKey(), pub)
	}
}

// TestFlasherBuilderRefusesOtherHostOS pins that the IncusOS-only image
// builder never bakes another OS's config into an IncusOS base image.
func TestFlasherBuilderRefusesOtherHostOS(t *testing.T) {
	b := flasherBuilder{basePath: "/nonexistent/base.img", binPath: "/nonexistent/flasher-tool"}
	err := b.Build(context.Background(), config.OSTalos, t.TempDir(), filepath.Join(t.TempDir(), "out.img"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `no base image for host OS "talos"`) {
		t.Fatalf("Build(talos) = %v, want a refusal naming the OS", err)
	}
}
