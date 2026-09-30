package main

import (
	"errors"
	"strings"
	"testing"
	"time"

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
		s.failureThreshold != defaultFailureThreshold || s.repoRef != "" {
		t.Errorf("defaults = %+v", s)
	}
}

func TestLoadSettingsOverrides(t *testing.T) {
	s, err := loadSettings(env(map[string]string{
		"AGENT_NODE_NAME":              "node1",
		"AGENT_INSTANCE_NAME":          "custom",
		"AGENT_GENERATION":             "2",
		"CONFIG_REPO_URL":              "/srv/fleet.git",
		"CONFIG_REPO_REF":              "prod",
		"AGENT_REPO_DIR":               "/tmp/x",
		"AGENT_INCUS_SOCKET":           "/tmp/incus.sock",
		"AGENT_TICK_INTERVAL":          "2s",
		"AGENT_SYNC_FAILURE_THRESHOLD": "4",
	}), host("ignored-g9"))
	if err != nil {
		t.Fatal(err)
	}
	want := settings{
		node: "node1", instance: "custom", generation: 2,
		repoURL: "/srv/fleet.git", repoRef: "prod", repoDir: "/tmp/x",
		socket: "/tmp/incus.sock", tick: 2 * time.Second, failureThreshold: 4,
	}
	if s != want {
		t.Errorf("settings = %+v, want %+v", s, want)
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
