// Package incusregistry is the Incus-backed leaderelection.Registry: each
// agent publishes its state as user.* keys on its own Incus instance, and
// reads every other agent's from one instance listing (docs/Decisions.md §25).
//
// # Which instances are peers
//
// A peer is an instance that
//   - internal/incuslocal.ListInstances returns, i.e. carries
//     user.homelab-ops.app — every App-managed instance does, the agent's
//     own included (#100, #203);
//   - carries the same user.homelab-ops.app value as this agent's own
//     instance: it's an instance of the same agent App, so of the same
//     fleet. Without this, every agent on one Incus elects together, so a
//     throwaway agent (a validate script's, or one following another config
//     repo) would fence real ones with commits from a history they don't
//     share and hold them off with its acting flag. An agent whose own
//     instance isn't listed has no App to match, so it has no peers;
//   - carries KeyNode, which only an agent's own Record writes. That's the
//     marker, rather than a naming prefix or the App's name: it's set by the
//     code that participates in election, so an instance counts from its
//     agent's first publication on, whatever the agent App is called; and
//   - Incus reports as exactly "Running". Not "not stopped": instances on an
//     offline cluster member report Stopped or Error, and the #212 handoff
//     relies on their leftover acting flag dropping out.
//
// An agent whose own instance fails the first or last test is invisible to
// its peers, so leaderelection.Designated refuses to lead from it.
//
// The App is read from the listing rather than configured, so it can't
// disagree with the tag the agent's own instance actually carries.
//
// # Single writer
//
// Every write goes to the agent's own instance, through incuslocal's
// unconditional SetUserKeys. That's safe only because nothing else writes
// these keys: one writer per instance, so there's no race to lose and no
// read-modify-write anywhere (#212 dropped the epoch ratchet that needed one).
package incusregistry

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/leaderelection"
)

// The keys an agent publishes on its own instance.
const (
	keyPrefix = "user.homelab-ops.agent."

	// KeyNode is the node (an Instance name from git config) the agent runs
	// on. Its presence is what marks an instance as an agent peer.
	KeyNode = keyPrefix + "node"
	// KeyGeneration is the agent instance's blue-green generation.
	KeyGeneration = keyPrefix + "generation"
	// KeyCommit is the git commit the agent's designation came from; empty
	// before its first successful sync.
	KeyCommit = keyPrefix + "commit"
	// KeyActing is "true" while the agent may be doing leader-only work.
	KeyActing = keyPrefix + "acting"
	// KeyDraining is "true" once the agent has stopped acting for good or
	// until it recovers.
	KeyDraining = keyPrefix + "draining"
	// KeyDrainReason says why KeyDraining is set (DrainSyncFailure, or
	// whatever set it), because only some reasons are recoverable (#187).
	KeyDrainReason = keyPrefix + "drain-reason"
	// KeyHeartbeat is the RFC 3339 UTC time of the agent's last tick:
	// observability only, never an election input.
	KeyHeartbeat = keyPrefix + "heartbeat"
	// KeyStatus is the agent's last leadership decision, human-readable, so
	// `incus list` shows why each agent is or isn't acting.
	KeyStatus = keyPrefix + "status"
)

// DrainSyncFailure is the drain reason for an agent whose git sync failed
// FailureThreshold times in a row (#187). Unlike a self-upgrade drain it's
// recoverable, subject to leaderelection.MayUndrain.
const DrainSyncFailure = "sync-failure"

// DrainSuperseded is the drain reason for an agent that drained on sync
// failure and found, on recovering, that a newer generation on its node had
// taken over. It stays drained until the leader retires it.
const DrainSuperseded = "superseded"

// running is the one Incus status that counts as a live peer.
const running = "Running"

// Client is the slice of *incuslocal.Client the registry uses.
type Client interface {
	ListInstances(ctx context.Context) ([]incuslocal.Instance, error)
	SetUserKeys(ctx context.Context, name string, keys map[string]string) error
}

var _ Client = (*incuslocal.Client)(nil)

// Registry publishes to and reads from Incus as one agent instance.
type Registry struct {
	Client Client
	// Self is this agent's Incus instance name; every write goes there.
	Self string
	// Node and Generation are published with every Record.
	Node       string
	Generation int64
}

var _ leaderelection.Registry = (*Registry)(nil)

// Record implements leaderelection.Registry. It writes the whole publication
// every time, node and generation included, so a record is never half-written
// by an earlier version of the agent.
func (r *Registry) Record(ctx context.Context, p leaderelection.Publication) error {
	return r.set(ctx, map[string]string{
		KeyNode:       r.Node,
		KeyGeneration: strconv.FormatInt(r.Generation, 10),
		KeyCommit:     p.Commit,
		KeyActing:     strconv.FormatBool(p.Acting),
	})
}

// Peers implements leaderelection.Registry: the published state of every
// Running agent instance of this agent's own App, keyed by instance name,
// this agent's own included.
//
// A value this package can't parse is an error, not a skipped peer: only
// agents write these keys, so a malformed one means something is wrong, and an
// error makes MayAct answer "not leader" rather than guess.
func (r *Registry) Peers(ctx context.Context) (map[string]leaderelection.Peer, error) {
	insts, err := r.Client.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	app, listed := "", false
	for _, inst := range insts {
		if inst.Name == r.Self {
			app, listed = inst.Config[incuslocal.AppTagKey], true
			break
		}
	}
	peers := make(map[string]leaderelection.Peer, len(insts))
	if !listed {
		return peers, nil
	}
	for _, inst := range insts {
		node, isAgent := inst.Config[KeyNode]
		if !isAgent || inst.Status != running || inst.Config[incuslocal.AppTagKey] != app {
			continue
		}
		p, err := parsePeer(node, inst.Config)
		if err != nil {
			return nil, fmt.Errorf("incusregistry: instance %s: %w", inst.Name, err)
		}
		peers[inst.Name] = p
	}
	return peers, nil
}

// Own reads this agent's own instance: its published state, its drain
// reason, and whether its peers can see it at all (listed and Running).
// Found is false before the agent's first Record.
func (r *Registry) Own(ctx context.Context) (s SelfState, err error) {
	insts, err := r.Client.ListInstances(ctx)
	if err != nil {
		return SelfState{}, err
	}
	for _, inst := range insts {
		if inst.Name != r.Self {
			continue
		}
		s.Listed, s.Status = true, inst.Status
		s.DrainReason = inst.Config[KeyDrainReason]
		if node, ok := inst.Config[KeyNode]; ok {
			s.Peer, err = parsePeer(node, inst.Config)
			if err != nil {
				return SelfState{}, fmt.Errorf("incusregistry: own instance %s: %w", r.Self, err)
			}
			s.Found = true
		}
		return s, nil
	}
	return s, nil
}

// SelfState is what Own reads back about this agent's own instance.
type SelfState struct {
	// Listed means ListInstances returns the instance (it carries the App
	// tag); Status is Incus's status for it.
	Listed bool
	Status string
	// Found means the agent has published on it before.
	Found       bool
	Peer        leaderelection.Peer
	DrainReason string
}

// VisibleToPeers reports whether other agents' Peers would include this one.
func (s SelfState) VisibleToPeers() bool { return s.Listed && s.Status == running }

// SetDraining publishes this agent's draining state and why. Clearing it
// clears the reason too.
func (r *Registry) SetDraining(ctx context.Context, draining bool, reason string) error {
	if !draining {
		reason = ""
	}
	return r.set(ctx, map[string]string{
		KeyDraining:    strconv.FormatBool(draining),
		KeyDrainReason: reason,
	})
}

// Heartbeat publishes the time of this tick and the agent's last decision.
func (r *Registry) Heartbeat(ctx context.Context, now time.Time, status string) error {
	return r.set(ctx, map[string]string{
		KeyHeartbeat: now.UTC().Format(time.RFC3339),
		KeyStatus:    status,
	})
}

func (r *Registry) set(ctx context.Context, keys map[string]string) error {
	if r.Client == nil || r.Self == "" {
		return errors.New("incusregistry: Client and Self must be set")
	}
	return r.Client.SetUserKeys(ctx, r.Self, keys)
}

func parsePeer(node string, cfg map[string]string) (leaderelection.Peer, error) {
	p := leaderelection.Peer{Node: node, Commit: cfg[KeyCommit]}
	var err error
	if p.Generation, err = parseInt(cfg, KeyGeneration); err != nil {
		return p, err
	}
	if p.Acting, err = parseBool(cfg, KeyActing); err != nil {
		return p, err
	}
	if p.Draining, err = parseBool(cfg, KeyDraining); err != nil {
		return p, err
	}
	return p, nil
}

// parseBool reads a published flag; absent or empty means false (never set).
func parseBool(cfg map[string]string, key string) (bool, error) {
	v := cfg[key]
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q: %w", key, v, err)
	}
	return b, nil
}

func parseInt(cfg map[string]string, key string) (int64, error) {
	v, ok := cfg[key]
	if !ok || v == "" {
		return 0, fmt.Errorf("%s is missing", key)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", key, v, err)
	}
	return n, nil
}
