// Command agent is the per-node app-manager agent (docs/AppManager.md). Every
// node runs the same binary; leaderelection.Designated decides each tick
// which one may act.
//
// It keeps a persistent clone of the config repo, publishes its commit and
// acting state on its own Incus instance, runs the #212 handoff, and writes a
// heartbeat (#101). When it leads, it creates the agent on every declared
// cluster member that has none (#203), and replaces every host's agent
// blue-green when the agent App's image changes in git (#109), each new agent
// syncing the same CONFIG_REPO_URL and CONFIG_REPO_REF as itself. Generic App
// reconciliation (#98) is deferred to Phase 8 (docs/Decisions.md §30).
//
// Configuration is environment variables only, in cmd/web's style:
//
//	AGENT_NODE_NAME                required: this node's Instance name in git config
//	AGENT_INSTANCE_NAME            this agent's Incus instance name (default: hostname)
//	AGENT_GENERATION               blue-green generation (default: parsed from a -g<N> instance-name suffix)
//	CONFIG_REPO_URL                required: the config repo
//	CONFIG_REPO_REF                branch to follow (default: main)
//	AGENT_REPO_DIR                 where the persistent clone lives (default: /var/lib/homelab-ops-agent/config-repo)
//	AGENT_INCUS_SOCKET             Incus unix socket (default: seed.IncusSocketPath)
//	AGENT_TICK_INTERVAL            e.g. 30s (default: 30s)
//	AGENT_SYNC_FAILURE_THRESHOLD   consecutive failed syncs before the designation is distrusted and the agent drains (default: 10)
//	AGENT_HEARTBEAT_STALE_AFTER    a self-upgrade candidate is healthy while its heartbeat is younger than this (default: 90s)
//	AGENT_MIN_HEALTHY_DURATION     how long a candidate stays healthy before it replaces the old agent (default: 2m)
//	AGENT_UPGRADE_HEALTH_DEADLINE  a candidate not sustained-healthy this long after its creation is deleted, and the attempt counted (default: 10m)
//	AGENT_UPGRADE_MAX_ATTEMPTS     counted attempts at one image before a host's upgrade is held, saying so in the status key (default: 3)
//
// AGENT_HEARTBEAT_STALE_AFTER must exceed the tick, and the deadline
// AGENT_MIN_HEALTHY_DURATION plus two ticks, or no candidate could ever pass
// (upgradeTimingErrs).
//
// The tick, threshold and four self-upgrade knobs are inherited: each agent
// this one creates gets this agent's own values (inheritedEnv), so an
// operator's tuning survives a self-upgrade.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/ehharvey/homelab-ops/internal/agent"
	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

const (
	defaultRepoDir          = "/var/lib/homelab-ops-agent/config-repo"
	defaultTickInterval     = 30 * time.Second
	defaultFailureThreshold = 10 // with the default tick, ~5 minutes: the project's RTO scale (#187)

	// socketWait bounds how long startup waits for the Incus socket to
	// appear; see dialWhenReady.
	socketWait = time.Minute
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
	upgrade          agent.Upgrade
}

// inherited lists the knobs an agent passes on to the agents it creates.
// Not the identity ones (node, instance, generation), nor where it finds its
// clone and socket: those are each instance's own.
var inherited = []string{
	"AGENT_TICK_INTERVAL",
	"AGENT_SYNC_FAILURE_THRESHOLD",
	"AGENT_HEARTBEAT_STALE_AFTER",
	"AGENT_MIN_HEALTHY_DURATION",
	"AGENT_UPGRADE_HEALTH_DEADLINE",
	"AGENT_UPGRADE_MAX_ATTEMPTS",
}

// inheritedEnv is the inherited knobs set in this agent's environment, for
// agent.Agent.ChildEnv. loadSettings has already validated them.
func inheritedEnv(getenv func(string) string) map[string]string {
	out := map[string]string{}
	for _, k := range inherited {
		if v := getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func run() error {
	s, err := loadSettings(os.Getenv, os.Hostname)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := dialWhenReady(ctx, s.socket, socketWait, time.Second)
	if errors.Is(err, context.Canceled) {
		return nil // stopped before the socket appeared: a clean stop, not a failed start
	}
	if err != nil {
		return err
	}
	clone := &configsync.Clone{RepoURL: s.repoURL, Ref: s.repoRef, Dir: s.repoDir, FailureThreshold: s.failureThreshold}
	reg := &incusregistry.Registry{Client: client, Self: s.instance, Node: s.node, Generation: s.generation}
	repo := agentdeploy.Repo{URL: s.repoURL, Ref: s.repoRef}
	a := agent.New(s.instance, s.node, s.generation, clone, reg, client, repo, logger)
	a.Upgrade = s.upgrade
	a.ChildEnv = inheritedEnv(os.Getenv)

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

// dialWhenReady dials the Incus socket, waiting up to wait for it to exist.
// The incus-socket profile's proxy is a post-start device: Incus creates the
// socket inside the container only once the container is running, so a
// freshly started agent can look before it is there (seen on a real node,
// #100). Any error other than the socket being absent fails at once, and a
// cancelled ctx returns ctx's error.
func dialWhenReady(ctx context.Context, path string, wait, poll time.Duration) (*incuslocal.Client, error) {
	deadline := time.Now().Add(wait)
	for {
		client, err := incuslocal.Dial(path)
		if err == nil || !errors.Is(err, fs.ErrNotExist) || time.Now().After(deadline) {
			return client, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
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

	s.upgrade = agent.DefaultUpgrade()
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"AGENT_HEARTBEAT_STALE_AFTER", &s.upgrade.HeartbeatStaleAfter},
		{"AGENT_MIN_HEALTHY_DURATION", &s.upgrade.MinHealthy},
		{"AGENT_UPGRADE_HEALTH_DEADLINE", &s.upgrade.HealthDeadline},
	} {
		if raw := getenv(d.key); raw != "" {
			v, err := time.ParseDuration(raw)
			if err != nil || v <= 0 {
				errs = append(errs, fmt.Errorf("%s %q: want a positive duration", d.key, raw))
			}
			*d.dst = v
		}
	}
	if raw := getenv("AGENT_UPGRADE_MAX_ATTEMPTS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("AGENT_UPGRADE_MAX_ATTEMPTS %q: want a positive integer", raw))
		}
		s.upgrade.MaxAttempts = n
	}
	if len(errs) == 0 {
		errs = append(errs, upgradeTimingErrs(s.tick, s.upgrade)...)
	}
	return s, errors.Join(errs...)
}

// upgradeTimingErrs rejects tuning that is valid knob by knob but makes every
// self-upgrade fail, since the agents this one creates inherit it. A
// candidate heartbeats once a tick, so a stale-after no longer than a tick
// reads it stale between beats. And its healthy streak starts at the first
// leader tick that sees it, a tick or so after it boots, and is only seen to
// reach MinHealthy at a leader tick, up to a tick after that: a deadline
// within MinHealthy plus two ticks of its creation deletes it first.
func upgradeTimingErrs(tick time.Duration, u agent.Upgrade) []error {
	var errs []error
	if u.HeartbeatStaleAfter <= tick {
		errs = append(errs, fmt.Errorf("AGENT_HEARTBEAT_STALE_AFTER (%v) must be longer than AGENT_TICK_INTERVAL (%v): a candidate heartbeats once a tick, so it would read stale between beats",
			u.HeartbeatStaleAfter, tick))
	}
	if u.MinHealthy+2*tick >= u.HealthDeadline {
		errs = append(errs, fmt.Errorf("AGENT_UPGRADE_HEALTH_DEADLINE (%v) must be longer than AGENT_MIN_HEALTHY_DURATION (%v) plus two AGENT_TICK_INTERVALs (%v): every candidate would be deleted before its healthy streak could be seen to reach the minimum",
			u.HealthDeadline, u.MinHealthy, 2*tick))
	}
	return errs
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
