package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/server"
	"github.com/ehharvey/homelab-ops/internal/store"
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
	done := make(chan struct{})
	go func() {
		pollSync(ctx, svc, time.Hour)
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
	t.Setenv("CLIENT_CERT_PATH", "")

	if cs := newCertSource(); cs != nil {
		t.Errorf("newCertSource with CLIENT_CERT_PATH unset = %v, want nil", cs)
	}
}

func TestNewTunnelSourceStartsAndPersistsIdentity(t *testing.T) {
	t.Setenv("WIREGUARD_PORT", "0") // let the OS pick a free port

	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	ts, err := newTunnelSource(context.Background(), st, "203.0.113.1:51820")
	if err != nil {
		t.Fatalf("newTunnelSource: %v", err)
	}
	if ts == nil {
		t.Fatal("newTunnelSource = nil, want non-nil")
	}
	defer ts.Close() //nolint:errcheck // test cleanup

	if ts.Endpoint() != "203.0.113.1:51820" {
		t.Errorf("Endpoint() = %q, want %q", ts.Endpoint(), "203.0.113.1:51820")
	}

	// A second call must reuse the persisted identity rather than minting a
	// new one, so a restarted process keeps the same public key every node's
	// seed already trusts.
	ts2, err := newTunnelSource(context.Background(), st, "203.0.113.1:51820")
	if err != nil {
		t.Fatalf("second newTunnelSource: %v", err)
	}
	defer ts2.Close() //nolint:errcheck // test cleanup

	if ts.PublicKey() != ts2.PublicKey() {
		t.Errorf("PublicKey() changed across newTunnelSource calls: %s != %s", ts.PublicKey(), ts2.PublicKey())
	}
}
