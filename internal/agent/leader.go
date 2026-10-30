package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
)

// leaderView is one leader tick's read of git and Incus. ensureAgents and
// upgradeAgents both act on it, so they decide from the same snapshot and a
// tick lists instances and members once, not once per pass.
//
// It is read before either pass acts. ensureAgents only creates, so the one
// thing upgradeAgents can't see in it is an agent ensureAgents just made: a
// create still running is in Agent.creating, which upgradeAgents skips, and
// a finished one is a host with nothing to upgrade yet.
type leaderView struct {
	snap      configsync.Snapshot
	app       config.App // the declared agent App
	instances []incuslocal.Instance
	own       incuslocal.Instance // this agent's own, tagged app.Name
	members   []incuslocal.Member // nil on an unclustered daemon, which has none
}

func (v leaderView) clustered() bool { return v.own.Location != incuslocal.UnclusteredLocation }

// leaderWork is Tick's leader-only work: #203's creates, then #109's
// upgrades, on one leaderView and within one passTimeout. It reports
// upgradeAgents' drained and holds.
func (a *Agent) leaderWork(ctx context.Context, commit string) (drained bool, holds []string) {
	pctx, cancel := context.WithTimeout(ctx, a.passTimeout)
	defer cancel()
	v, ok := a.readLeaderView(pctx, commit)
	if !ok {
		return false, nil
	}
	a.ensureAgents(ctx, pctx, v)
	return a.upgradeAgents(ctx, pctx, v)
}

// readLeaderView reads what both passes act on, and reports false, with the
// reason logged, when neither may act this tick.
//
// commit is the commit MayAct decided on. The config read here must be that
// same checkout, or the leader would act on config it hasn't been fenced on.
func (a *Agent) readLeaderView(ctx context.Context, commit string) (leaderView, bool) {
	snap, err := a.sync.Current()
	if err != nil {
		a.log.Printf("member agents: read config: %v (retrying next tick)", err)
		return leaderView{}, false
	}
	if snap.Commit != commit {
		a.log.Printf("member agents: checkout is at %s, not the decided %s (retrying next tick)", snap.Commit, commit)
		return leaderView{}, false
	}
	app, ok := agentdeploy.FindApp(snap.Config.Apps)
	if !ok {
		a.note("app", fmt.Sprintf("member agents: no agent App declared (kind: App, type: %s, replicas: per-node); creating none", agentdeploy.AppType))
		a.pruneNotes(nil)
		return leaderView{}, false
	}

	// Before the listing, so a create that finished since the last tick is
	// in it rather than sent again.
	a.settleCreates(ctx)

	instances, err := a.incus.ListInstances(ctx)
	if err != nil {
		a.log.Printf("member agents: %v (retrying next tick)", err)
		return leaderView{}, false
	}
	i := slices.IndexFunc(instances, func(inst incuslocal.Instance) bool { return inst.Name == a.self })
	if i < 0 {
		a.note("app", fmt.Sprintf("member agents: own instance %s isn't listed (it needs the %s tag); creating none", a.self, incuslocal.AppTagKey))
		a.pruneNotes(nil)
		return leaderView{}, false
	}
	own := instances[i]
	// The agents this one creates must be its peers: Peers groups agents by
	// the tag on their own instance, so an agent tagged otherwise than
	// app.Name (an App renamed in git, a throwaway agent) would create a
	// second, separate set of agents, with a second leader among them.
	if tag := own.Config[incuslocal.AppTagKey]; tag != app.Name {
		a.note("app", fmt.Sprintf("member agents: own instance %s has %s=%q, but the declared agent App is %q; creating none (its agents would not be this agent's peers)",
			a.self, incuslocal.AppTagKey, tag, app.Name))
		a.pruneNotes(nil)
		return leaderView{}, false
	}
	a.note("app", "")

	v := leaderView{snap: snap, app: app, instances: instances, own: own}
	if v.clustered() {
		if v.members, err = a.incus.ListMembers(ctx); err != nil {
			a.log.Printf("member agents: %v (retrying next tick)", err)
			return leaderView{}, false
		}
	}
	return v, true
}
