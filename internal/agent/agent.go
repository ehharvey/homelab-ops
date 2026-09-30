// Package agent is the app-manager agent's tick loop (docs/AppManager.md):
// sync the config repo, publish, decide who leads, and keep this instance's
// acting and draining flags honest. cmd/agent wires it to the environment, a
// real configsync.Clone and a real incusregistry.Registry.
//
// It lives here rather than in cmd/agent so the drain state machine (#187)
// and the acting handoff (#212) can be reused by the self-upgrade handoff
// (#109), which drains for a reason of its own.
package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
	"github.com/ehharvey/homelab-ops/internal/leaderelection"
)

// Syncer is the slice of *configsync.Clone the loop uses.
type Syncer interface {
	Sync(ctx context.Context) (configsync.Snapshot, error)
	Current() (configsync.Snapshot, error)
	HasCommit(commit string) (bool, error)
	Failing() bool
}

var _ Syncer = (*configsync.Clone)(nil)

// Registry is the slice of *incusregistry.Registry the loop uses.
type Registry interface {
	leaderelection.Registry
	Own(ctx context.Context) (incusregistry.SelfState, error)
	SetDraining(ctx context.Context, draining bool, reason string) error
	Heartbeat(ctx context.Context, now time.Time, status string) error
}

var _ Registry = (*incusregistry.Registry)(nil)

// Agent is one app-manager agent's tick loop: sync, publish, decide, and —
// when it leads — nothing yet beyond saying so (#101 is deliberately minimal;
// App reconciliation is #98, paused under docs/Decisions.md §27).
//
// Not safe for concurrent use: one goroutine calls Tick, then Shutdown.
type Agent struct {
	self, node string
	generation int64

	sync    Syncer
	reg     Registry
	elector *leaderelection.Designated
	now     func() time.Time
	log     *log.Logger

	started     bool
	draining    bool   // as published on our own instance
	drainReason string // as published
	leading     bool   // as last logged, for transition logs only
}

// New returns the loop for agent instance self, generation generation, on
// node node.
func New(self, node string, generation int64, s Syncer, r Registry, logger *log.Logger) *Agent {
	return &Agent{
		self: self, node: node, generation: generation,
		sync: s, reg: r, now: time.Now, log: logger,
		elector: &leaderelection.Designated{
			Self: self, Node: node, Generation: generation,
			Source: func() (leaderelection.Designation, error) {
				snap, err := s.Current()
				if err != nil {
					return leaderelection.Designation{}, err
				}
				return leaderelection.Designation{Primary: snap.Config.Primary(), Commit: snap.Commit}, nil
			},
			HasCommit: s.HasCommit,
			Peers:     r,
		},
	}
}

// start clears this agent's own acting flag before anything else — a flag
// left by a previous run is its own, and it isn't acting — then adopts
// whatever drain state its instance already carries.
func (a *Agent) start(ctx context.Context) error {
	if err := a.elector.Stop(ctx); err != nil {
		return fmt.Errorf("clear own acting flag: %w", err)
	}
	own, err := a.reg.Own(ctx)
	if err != nil {
		return fmt.Errorf("read own instance: %w", err)
	}
	a.draining, a.drainReason = own.Peer.Draining, own.DrainReason
	if !own.VisibleToPeers() {
		// Not fatal: it may be a moment from Running. But until it's both
		// listed and Running, MayAct won't lead from it.
		a.log.Printf("warning: own instance %s is not visible to peers (listed=%v status=%q); it needs the %s tag and to be Running",
			a.self, own.Listed, own.Status, incuslocal.AppTagKey)
	}
	a.log.Printf("started as %s on node %s, generation %d (draining=%v, reason=%q)", a.self, a.node, a.generation, a.draining, a.drainReason)
	a.started = true
	return nil
}

// Tick is one pass of the loop. It never returns an error: every failure is
// logged and folds into "not leader" for this tick.
func (a *Agent) Tick(ctx context.Context) {
	if !a.started {
		if err := a.start(ctx); err != nil {
			a.log.Printf("startup: %v (retrying next tick)", err)
			return
		}
	}

	synced := a.syncOnce(ctx)
	a.drainOnSyncFailure(ctx, synced)
	dec := a.decide(ctx)
	if dec.Behind {
		// A peer is on a commit we lack: fetch now rather than on the next
		// poll, so a push pauses the leader for about one fetch (#212).
		a.log.Printf("behind a peer; re-syncing now")
		a.syncOnce(ctx)
		dec = a.decide(ctx)
	}

	status := dec.Reason
	if dec.Leader {
		// Leader-only work goes here, and must be Incus API calls through
		// this agent's own member: Incus quorum, not MayAct, is what stops a
		// partitioned agent from completing them. Work outside Incus needs a
		// fence of its own first (docs/Decisions.md §25).
		status = "leader: " + dec.Reason
	}
	if err := a.reg.Heartbeat(ctx, a.now(), status); err != nil && ctx.Err() == nil {
		a.log.Printf("heartbeat: %v", err)
	}
}

// syncOnce syncs and reports whether that sync succeeded.
func (a *Agent) syncOnce(ctx context.Context) bool {
	if _, err := a.sync.Sync(ctx); err != nil {
		a.log.Printf("sync: %v", err)
		return false
	}
	return true
}

// decide asks the elector, and keeps the acting flag honest: after a
// non-leader answer it stops (there's no leader work in flight yet to wait
// for) and only then clears the flag.
func (a *Agent) decide(ctx context.Context) leaderelection.Decision {
	dec, err := a.elector.MayAct(ctx)
	if err != nil {
		dec = leaderelection.Decision{Reason: "unknown: " + err.Error()}
	}

	if dec.Leader {
		if !a.leading {
			a.log.Printf("ACTING begin commit=%s: %s", dec.Commit, dec.Reason)
			a.leading = true
		}
		return dec
	}
	if a.leading {
		a.log.Printf("ACTING end: %s", dec.Reason)
		a.leading = false
	}
	if a.elector.Acting() {
		// Leader-only work would be stopped or abandoned here. There is none
		// yet, so the flag can be cleared at once.
		if err := a.elector.Stop(ctx); err != nil {
			a.log.Printf("clear acting flag: %v (retrying next tick)", err)
		}
	}
	return dec
}

// drainOnSyncFailure implements #187: once sync has failed
// FailureThreshold times in a row, mark this instance draining, durably and
// visibly; once a sync succeeds again, clear it — unless a newer generation
// on this node took over meanwhile, in which case stay drained for good.
//
// Recovery means synced, this tick's sync succeeding, not merely "not Failing
// yet": a restarted agent's failure count starts at zero, so an agent that
// restarts while drained and still can't reach git would otherwise un-drain
// on its first tick and drain again a threshold later, on every restart.
//
// Only a sync-failure drain is ever cleared here. A drain for any other
// reason (a self-upgrade handoff, #109) is permanent.
func (a *Agent) drainOnSyncFailure(ctx context.Context, synced bool) {
	switch {
	case a.sync.Failing() && !a.draining:
		if err := a.reg.SetDraining(ctx, true, incusregistry.DrainSyncFailure); err != nil {
			a.log.Printf("self-drain: %v", err)
			return
		}
		a.draining, a.drainReason = true, incusregistry.DrainSyncFailure
		a.log.Printf("draining: git sync keeps failing")

	case synced && a.draining && a.drainReason == incusregistry.DrainSyncFailure:
		peers, err := a.reg.Peers(ctx)
		if err != nil {
			a.log.Printf("un-drain: reading peers: %v", err)
			return
		}
		ok, why := leaderelection.MayUndrain(a.self, a.node, a.generation, peers)
		reason := ""
		if !ok {
			reason = incusregistry.DrainSuperseded
		}
		if err := a.reg.SetDraining(ctx, !ok, reason); err != nil {
			a.log.Printf("un-drain: %v", err)
			return
		}
		a.draining, a.drainReason = !ok, reason
		a.log.Printf("git sync recovered; draining=%v: %s", !ok, why)
	}
}

// Shutdown clears the acting flag once the loop has stopped: the only point
// at which this process is certain it isn't acting.
func (a *Agent) Shutdown(ctx context.Context) {
	if !a.elector.Acting() {
		return
	}
	if a.leading {
		a.log.Printf("ACTING end: shutting down")
		a.leading = false
	}
	if err := a.elector.Stop(ctx); err != nil {
		a.log.Printf("shutdown: clear acting flag: %v", err)
	}
}
