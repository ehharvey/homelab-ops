package agent

import (
	"context"

	"github.com/ehharvey/homelab-ops/internal/clusterruntime"
)

// LeaderPass is one leader tick's work, on one leaderView and within one
// passTimeout: what the cluster runtime's Join and Upgrade
// (clusterruntime.Runtime, docs/Decisions.md §31) act on.
//
// Its fields are opaque: a runtime can only run the passes this package
// gives it, so leaderWork's order, its passTimeout and the §25 gating stay
// here, in Tick and leaderWork, whatever runtime is set.
type LeaderPass struct {
	a         *Agent
	ctx, pctx context.Context
	v         leaderView
}

var _ clusterruntime.LeaderPass = LeaderPass{}

// EnsureAgents is ensureAgents: create the agent on every declared, Online
// cluster member that has none (#203).
func (p LeaderPass) EnsureAgents() { p.a.ensureAgents(p.ctx, p.pctx, p.v) }

// UpgradeAgents is upgradeAgents: replace every host's agent blue-green when
// the agent App's image changes in git (#109). It reports drained when this
// instance just drained for its candidate, and holds for its status.
func (p LeaderPass) UpgradeAgents() (drained bool, holds []string) {
	return p.a.upgradeAgents(p.ctx, p.pctx, p.v)
}

// Candidate is one host's self-upgrade candidate, as upgradeHost asks the
// cluster runtime's Health about it.
type Candidate struct {
	a    *Agent
	ctx  context.Context
	v    leaderView
	h    upgradeHost
	cand agentInstance
}

var _ clusterruntime.Candidate = Candidate{}

// SustainedHealthy is sustainedHealthy: keep the candidate's healthy streak
// (unhealthy, on its incusregistry.KeyHeartbeat) on this agent's own
// instance, and report whether it has held for Upgrade.MinHealthy.
func (c Candidate) SustainedHealthy() bool {
	return c.a.sustainedHealthy(c.ctx, c.v, c.h, c.cand)
}

// runtime is Agent.Runtime, or clusterruntime.Incus when it's nil: the
// runtime whose agent this is.
func (a *Agent) runtime() clusterruntime.Runtime {
	if a.Runtime != nil {
		return a.Runtime
	}
	return clusterruntime.Incus
}
