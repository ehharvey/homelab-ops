package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

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
	svc := server.NewService(cs, st)

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
