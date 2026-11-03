package main

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/agent"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func host(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

func TestLoadSettingsDefaults(t *testing.T) {
	s, err := loadSettings(env(map[string]string{
		"AGENT_NODE_NAME": "node0",
		"CONFIG_REPO_URL": "https://example.invalid/fleet.git",
	}), host("agent-node0-g7"))
	if err != nil {
		t.Fatal(err)
	}
	if s.instance != "agent-node0-g7" || s.generation != 7 {
		t.Errorf("instance/generation = %q/%d, want the hostname and its -g7 suffix", s.instance, s.generation)
	}
	if s.socket != seed.IncusSocketPath || s.repoDir != defaultRepoDir || s.tick != defaultTickInterval ||
		s.failureThreshold != defaultFailureThreshold || s.repoRef != "" || s.upgrade != agent.DefaultUpgrade() {
		t.Errorf("defaults = %+v", s)
	}
}

func TestLoadSettingsOverrides(t *testing.T) {
	s, err := loadSettings(env(map[string]string{
		"AGENT_NODE_NAME":               "node1",
		"AGENT_INSTANCE_NAME":           "custom",
		"AGENT_GENERATION":              "2",
		"CONFIG_REPO_URL":               "/srv/fleet.git",
		"CONFIG_REPO_REF":               "prod",
		"AGENT_REPO_DIR":                "/tmp/x",
		"AGENT_INCUS_SOCKET":            "/tmp/incus.sock",
		"AGENT_TICK_INTERVAL":           "2s",
		"AGENT_SYNC_FAILURE_THRESHOLD":  "4",
		"AGENT_HEARTBEAT_STALE_AFTER":   "6s",
		"AGENT_MIN_HEALTHY_DURATION":    "10s",
		"AGENT_UPGRADE_HEALTH_DEADLINE": "3m",
		"AGENT_UPGRADE_MAX_ATTEMPTS":    "5",
	}), host("ignored-g9"))
	if err != nil {
		t.Fatal(err)
	}
	want := settings{
		node: "node1", instance: "custom", generation: 2,
		repoURL: "/srv/fleet.git", repoRef: "prod", repoDir: "/tmp/x",
		socket: "/tmp/incus.sock", tick: 2 * time.Second, failureThreshold: 4,
		upgrade: agent.Upgrade{HeartbeatStaleAfter: 6 * time.Second, MinHealthy: 10 * time.Second, HealthDeadline: 3 * time.Minute, MaxAttempts: 5},
	}
	if s != want {
		t.Errorf("settings = %+v, want %+v", s, want)
	}
}

// The tuning knobs reach the agents this one creates; its identity, clone
// and socket don't, since those are each instance's own.
func TestInheritedEnvIsTheTuningKnobsOnly(t *testing.T) {
	got := inheritedEnv(env(map[string]string{
		"AGENT_NODE_NAME":            "node0",
		"AGENT_INSTANCE_NAME":        "agent-node0-g0",
		"AGENT_GENERATION":           "0",
		"AGENT_REPO_DIR":             "/tmp/x",
		"AGENT_INCUS_SOCKET":         "/tmp/s",
		"CONFIG_REPO_URL":            "/r",
		"AGENT_TICK_INTERVAL":        "2s",
		"AGENT_MIN_HEALTHY_DURATION": "10s",
	}))
	want := map[string]string{"AGENT_TICK_INTERVAL": "2s", "AGENT_MIN_HEALTHY_DURATION": "10s"}
	if !maps.Equal(got, want) {
		t.Errorf("inheritedEnv = %v, want %v", got, want)
	}
}

func TestLoadSettingsRejects(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"AGENT_NODE_NAME": "node0", "CONFIG_REPO_URL": "/r", "AGENT_INSTANCE_NAME": "agent-node0-g0"}
	}
	tests := []struct {
		name, key, value, want string
	}{
		{"missing node", "AGENT_NODE_NAME", "", "AGENT_NODE_NAME"},
		{"missing repo", "CONFIG_REPO_URL", "", "CONFIG_REPO_URL"},
		{"no generation anywhere", "AGENT_INSTANCE_NAME", "agent-node0", "-g<N>"},
		{"bad generation", "AGENT_GENERATION", "-1", "AGENT_GENERATION"},
		{"bad tick", "AGENT_TICK_INTERVAL", "0s", "AGENT_TICK_INTERVAL"},
		{"bad threshold", "AGENT_SYNC_FAILURE_THRESHOLD", "0", "AGENT_SYNC_FAILURE_THRESHOLD"},
		{"bad stale-after", "AGENT_HEARTBEAT_STALE_AFTER", "soon", "AGENT_HEARTBEAT_STALE_AFTER"},
		{"bad min healthy", "AGENT_MIN_HEALTHY_DURATION", "-1m", "AGENT_MIN_HEALTHY_DURATION"},
		{"bad deadline", "AGENT_UPGRADE_HEALTH_DEADLINE", "0s", "AGENT_UPGRADE_HEALTH_DEADLINE"},
		{"bad max attempts", "AGENT_UPGRADE_MAX_ATTEMPTS", "0", "AGENT_UPGRADE_MAX_ATTEMPTS"},
		// Each valid alone, but every upgrade would fail with the defaults.
		{"min healthy as long as the deadline", "AGENT_MIN_HEALTHY_DURATION", "10m", "AGENT_UPGRADE_HEALTH_DEADLINE"},
		{"deadline within min healthy plus two ticks", "AGENT_UPGRADE_HEALTH_DEADLINE", "2m59s", "two AGENT_TICK_INTERVALs"},
		{"tick as long as stale-after", "AGENT_TICK_INTERVAL", "90s", "AGENT_HEARTBEAT_STALE_AFTER"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := base()
			e[tt.key] = tt.value
			_, err := loadSettings(env(e), host(""))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one mentioning %s", err, tt.want)
			}
		})
	}

	t.Run("no instance name and no hostname", func(t *testing.T) {
		e := base()
		delete(e, "AGENT_INSTANCE_NAME")
		_, err := loadSettings(env(e), func() (string, error) { return "", errors.New("nope") })
		if err == nil || !strings.Contains(err.Error(), "AGENT_INSTANCE_NAME") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("no instance name and an empty hostname", func(t *testing.T) {
		e := base()
		delete(e, "AGENT_INSTANCE_NAME")
		_, err := loadSettings(env(e), host(""))
		if err == nil || !strings.Contains(err.Error(), "hostname is empty") || strings.Contains(err.Error(), "<nil>") {
			t.Errorf("err = %v, want it to say the hostname is empty", err)
		}
	})
}

// shortSocketPath is a unix socket path under the ~108-byte limit, which
// t.TempDir() can exceed with a long test name.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	if len(path) > 100 {
		t.Fatalf("socket path too long for a unix socket (%d bytes): %s", len(path), path)
	}
	return path
}

func TestDialWhenReadyWaitsForTheSocket(t *testing.T) {
	path := shortSocketPath(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		t.Cleanup(func() { _ = ln.Close() })
	}()

	if _, err := dialWhenReady(context.Background(), path, 5*time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("dialWhenReady with a socket that appears late = %v, want nil", err)
	}
}

func TestDialWhenReadyGivesUp(t *testing.T) {
	path := shortSocketPath(t)
	_, err := dialWhenReady(context.Background(), path, 30*time.Millisecond, 10*time.Millisecond)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dialWhenReady with no socket = %v, want fs.ErrNotExist after the wait", err)
	}
}

func TestDialWhenReadyStopsOnCancel(t *testing.T) {
	path := shortSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dialWhenReady(ctx, path, 5*time.Second, 10*time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("dialWhenReady after a stop signal = %v, want context.Canceled, not the socket's stat error", err)
	}
}

func TestDialWhenReadyFailsFastOnANonSocket(t *testing.T) {
	path := shortSocketPath(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := dialWhenReady(context.Background(), path, 5*time.Second, 10*time.Millisecond); err == nil {
		t.Fatal("dialWhenReady on a regular file = nil, want an error")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("dialWhenReady waited %v on a non-socket; only an absent socket should be waited for", waited)
	}
}
