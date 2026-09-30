// Command agent is the per-node app-manager agent (docs/AppManager.md). Every
// node runs the same binary; leaderelection.Designated decides each tick
// which one may act.
//
// This is the minimal agent of #101: it keeps a persistent clone of the config
// repo, publishes its commit and acting state on its own Incus instance, runs
// the #212 handoff, and writes a heartbeat. When it leads it does nothing yet
// but log — its first real job is cluster membership (#180), and App
// reconciliation (#98) is paused (docs/Decisions.md §27).
//
// Configuration is environment variables only, in cmd/web's style:
//
//	AGENT_NODE_NAME               required: this node's Instance name in git config
//	AGENT_INSTANCE_NAME           this agent's Incus instance name (default: hostname)
//	AGENT_GENERATION              blue-green generation (default: parsed from a -g<N> instance-name suffix)
//	CONFIG_REPO_URL               required: the config repo
//	CONFIG_REPO_REF               branch to follow (default: main)
//	AGENT_REPO_DIR                where the persistent clone lives (default: /var/lib/homelab-ops-agent/config-repo)
//	AGENT_INCUS_SOCKET            Incus unix socket (default: seed.IncusSocketPath)
//	AGENT_TICK_INTERVAL           e.g. 30s (default: 30s)
//	AGENT_SYNC_FAILURE_THRESHOLD  consecutive failed syncs before the designation is distrusted and the agent drains (default: 10)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/ehharvey/homelab-ops/internal/agent"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

const (
	defaultRepoDir          = "/var/lib/homelab-ops-agent/config-repo"
	defaultTickInterval     = 30 * time.Second
	defaultFailureThreshold = 10 // with the default tick, ~5 minutes: the project's RTO scale (#187)
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// settings is the agent's parsed environment.
type settings struct {
	node, instance   string
	generation       int64
	repoURL, repoRef string
	repoDir          string
	socket           string
	tick             time.Duration
	failureThreshold int
}

func run() error {
	s, err := loadSettings(os.Getenv, os.Hostname)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	client, err := incuslocal.Dial(s.socket)
	if err != nil {
		return err
	}
	clone := &configsync.Clone{RepoURL: s.repoURL, Ref: s.repoRef, Dir: s.repoDir, FailureThreshold: s.failureThreshold}
	reg := &incusregistry.Registry{Client: client, Self: s.instance, Node: s.node, Generation: s.generation}
	a := agent.New(s.instance, s.node, s.generation, clone, reg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		a.Tick(ctx)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			a.Shutdown(shutdownCtx)
			cancel()
			return nil
		case <-ticker.C:
		}
	}
}

// generationSuffix matches the -g<N> suffix of an agent instance name, e.g.
// agent-node0-g3 (docs/AppManager.md's naming).
var generationSuffix = regexp.MustCompile(`-g([0-9]+)$`)

// loadSettings reads the environment. Unlike cmd/web, which degrades an
// invalid optional value to its default, anything set but invalid is an
// error: an agent that silently ran with the wrong generation or node could
// act when it shouldn't.
func loadSettings(getenv func(string) string, hostname func() (string, error)) (settings, error) {
	s := settings{
		node:     getenv("AGENT_NODE_NAME"),
		instance: getenv("AGENT_INSTANCE_NAME"),
		repoURL:  getenv("CONFIG_REPO_URL"),
		repoRef:  getenv("CONFIG_REPO_REF"),
		repoDir:  orDefault(getenv("AGENT_REPO_DIR"), defaultRepoDir),
		socket:   orDefault(getenv("AGENT_INCUS_SOCKET"), seed.IncusSocketPath),
	}
	var errs []error
	if s.node == "" {
		errs = append(errs, errors.New("AGENT_NODE_NAME is required"))
	}
	if s.repoURL == "" {
		errs = append(errs, errors.New("CONFIG_REPO_URL is required"))
	}
	if s.instance == "" {
		h, err := hostname()
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("AGENT_INSTANCE_NAME is unset and the hostname is unavailable: %w", err))
		case h == "":
			errs = append(errs, errors.New("AGENT_INSTANCE_NAME is unset and the hostname is empty"))
		}
		s.instance = h
	}

	if raw := getenv("AGENT_GENERATION"); raw != "" {
		g, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || g < 0 {
			errs = append(errs, fmt.Errorf("AGENT_GENERATION %q: want a non-negative integer", raw))
		}
		s.generation = g
	} else if m := generationSuffix.FindStringSubmatch(s.instance); m != nil {
		g, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("generation suffix of %q: %w", s.instance, err))
		}
		s.generation = g
	} else if s.instance != "" {
		errs = append(errs, fmt.Errorf("AGENT_GENERATION is unset and instance name %q has no -g<N> suffix", s.instance))
	}

	s.tick = defaultTickInterval
	if raw := getenv("AGENT_TICK_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("AGENT_TICK_INTERVAL %q: want a positive duration", raw))
		}
		s.tick = d
	}
	s.failureThreshold = defaultFailureThreshold
	if raw := getenv("AGENT_SYNC_FAILURE_THRESHOLD"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("AGENT_SYNC_FAILURE_THRESHOLD %q: want a positive integer", raw))
		}
		s.failureThreshold = n
	}
	return s, errors.Join(errs...)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
