package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
)

// Incus is the slice of *incuslocal.Client the leader's work uses.
type Incus interface {
	ListMembers(ctx context.Context) ([]incuslocal.Member, error)
	ListInstances(ctx context.Context) ([]incuslocal.Instance, error)
	CreateInstance(ctx context.Context, req lxcapi.InstancesPost, target string) error
	Operation(ctx context.Context, id string) (*lxcapi.Operation, error)
}

var _ Incus = (*incuslocal.Client)(nil)

// defaultPassTimeout is Agent.passTimeout: it bounds one pass, every
// member's create and image pull included. Tick runs on the process's
// lifetime ctx, and creates run one after another, so stalled pulls would
// otherwise hold the tick, and the acting flag a #212 handoff waits on, for
// as long as they stall. A create still running when its wait ends isn't
// cancelled in Incus: it is remembered by operation ID and looked up, not
// sent again (Agent.creating).
const defaultPassTimeout = 5 * time.Minute

// ensureAgents is #98's zero-match branch carved out for the agent App alone
// (docs/Decisions.md §27, §30, #203): every declared, Online cluster member
// without an agent gets generation 0 of one, placed on it with
// target=<member>. It is leader-only, and every step is an Incus API call
// through this agent's own member, so Incus quorum fences it (§25).
//
// Nothing else of #98 comes with it: an existing agent is left alone whatever
// its image or status (upgrades are #109's), and nothing is ever deleted, not
// even an agent on the wrong member. Every skip and failed create is retried
// next tick; a create still running in Incus is waited on, never re-sent; a
// member whose agent exists but isn't Running is logged and left for the
// operator, since a create can't fix it.
//
// commit is the commit MayAct decided on. The config read here must be that
// same checkout, or the leader would act on config it hasn't been fenced on.
func (a *Agent) ensureAgents(ctx context.Context, commit string) {
	snap, err := a.sync.Current()
	if err != nil {
		a.log.Printf("member agents: read config: %v (retrying next tick)", err)
		return
	}
	if snap.Commit != commit {
		a.log.Printf("member agents: checkout is at %s, not the decided %s (retrying next tick)", snap.Commit, commit)
		return
	}
	app, ok := agentdeploy.FindApp(snap.Config.Apps)
	if !ok {
		a.note("app", fmt.Sprintf("member agents: no agent App declared (kind: App, type: %s, replicas: per-node); creating none", agentdeploy.AppType))
		a.pruneNotes(nil)
		return
	}

	pctx, cancel := context.WithTimeout(ctx, a.passTimeout)
	defer cancel()
	// Before the listing, so a create that finished since the last tick is
	// in it rather than sent again.
	a.settleCreates(pctx)

	instances, err := a.incus.ListInstances(pctx)
	if err != nil {
		a.log.Printf("member agents: %v (retrying next tick)", err)
		return
	}
	i := slices.IndexFunc(instances, func(inst incuslocal.Instance) bool { return inst.Name == a.self })
	if i < 0 {
		a.note("app", fmt.Sprintf("member agents: own instance %s isn't listed (it needs the %s tag); creating none", a.self, incuslocal.AppTagKey))
		a.pruneNotes(nil)
		return
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
		return
	}
	a.note("app", "")
	// An unclustered daemon has no members to list (the call errors) and
	// rejects any target. Its one host is a standalone host, whose agent is
	// this one (§30), so there is nothing to create.
	if own.Location == incuslocal.UnclusteredLocation {
		a.note("cluster", "member agents: Incus is not clustered; a standalone host's only agent is this one")
		a.pruneNotes(nil)
		return
	}
	a.note("cluster", "")

	members, err := a.incus.ListMembers(pctx)
	if err != nil {
		a.log.Printf("member agents: %v (retrying next tick)", err)
		return
	}

	// A member has an agent when an instance carries the agent App's tag
	// and sits on that member. The name alone can't say: instance names are
	// cluster-wide, so agent-node1-g0 may be sitting on node2.
	running := map[string]bool{} // member -> whether any of its agents is Running
	for _, inst := range instances {
		if inst.Config[incuslocal.AppTagKey] == app.Name {
			running[inst.Location] = running[inst.Location] || inst.Status == incuslocal.StatusRunning
		}
	}
	declared := map[string]config.Instance{}
	for _, inst := range snap.Config.Instances {
		declared[inst.Name] = inst
	}

	// Each note key this pass states; the rest belong to members and
	// Instances that are gone, and are forgotten below.
	seen := map[string]bool{}
	note := func(key, msg string) {
		seen[key] = true
		a.note(key, msg)
	}

	slices.SortFunc(members, func(x, y incuslocal.Member) int { return strings.Compare(x.Name, y.Name) })
	isMember := map[string]bool{}
	for _, m := range members {
		isMember[m.Name] = true
		key := "member/" + m.Name
		inst, ok := declared[m.Name]
		isRunning, hasAgent := running[m.Name]
		creating, isCreating := a.creating[m.Name]
		switch {
		case !ok:
			note(key, fmt.Sprintf("member agents: member %s is not a declared Instance; skipping it", m.Name))
			continue
		case m.Status != incuslocal.MemberOnline:
			note(key, fmt.Sprintf("member agents: member %s is %s, not %s; skipping it", m.Name, m.Status, incuslocal.MemberOnline))
			continue
		case isCreating:
			// Ahead of the not-Running case: Incus lists the instance,
			// Stopped, while the create that will start it is still running.
			note(key, fmt.Sprintf("member agents: the create on member %s (operation %s) is still running; waiting for it", m.Name, creating))
			continue
		case hasAgent && !isRunning:
			// A create that made the instance but failed to start it, or an
			// agent the operator stopped: either way not this loop's to start.
			note(key, fmt.Sprintf("member agents: member %s has an agent, but none is %s; leaving it (start it, or delete it for the primary to recreate)", m.Name, incuslocal.StatusRunning))
			continue
		}
		note(key, "")
		if hasAgent {
			continue
		}

		if ctx.Err() != nil {
			return // shutting down: there is no next tick to retry on
		}
		if pctx.Err() != nil {
			a.log.Printf("member agents: this pass's %v is spent; the remaining members wait for next tick", a.passTimeout)
			return
		}
		req := agentdeploy.Desired(app, inst, a.repo)
		// Logged every tick, not only on change: a create that keeps failing
		// (an agent of this name on another member, an image this member
		// can't pull) needs the operator.
		err := a.incus.CreateInstance(pctx, req, m.Name)
		var pending *incuslocal.OperationPendingError
		switch {
		case errors.As(err, &pending):
			a.creating[m.Name] = pending.ID
			a.log.Printf("member agents: create %s on %s is still running (operation %s); checking on it next tick", req.Name, m.Name, pending.ID)
		case err != nil:
			a.log.Printf("member agents: create %s on %s: %v (retrying next tick)", req.Name, m.Name, err)
		default:
			a.log.Printf("member agents: created %s on member %s", req.Name, m.Name)
		}
	}

	for _, inst := range snap.Config.Instances {
		key := "instance/" + inst.Name
		if isMember[inst.Name] {
			note(key, "")
			continue
		}
		// Not "yet": a standalone host (§30) never joins, and its agent
		// comes from the web route or `bootstrap deploy-agent`.
		note(key, fmt.Sprintf("member agents: declared Instance %s is not a member of this cluster; skipping it", inst.Name))
	}

	a.pruneNotes(seen)
}

// settleCreates looks up each remembered create (a.creating) and forgets the
// ones Incus has finished, logging a failure. One it can't look up stays
// remembered: sending it again would only race the original.
func (a *Agent) settleCreates(ctx context.Context) {
	for member, id := range a.creating {
		op, err := a.incus.Operation(ctx, id)
		switch {
		case errors.Is(err, incuslocal.ErrOperationGone):
			// Finished; the listing shows whether it made the agent.
		case err != nil:
			a.log.Printf("member agents: look up the create on %s: %v (retrying next tick)", member, err)
			continue
		case !op.StatusCode.IsFinal():
			continue
		case op.StatusCode != lxcapi.Success:
			a.log.Printf("member agents: the create on %s (operation %s) ended %s: %s (retrying next tick)", member, id, op.Status, op.Err)
		}
		delete(a.creating, member)
	}
}

// pruneNotes forgets the member/ and instance/ notes not in seen, the keys a
// full pass stated. A pass that creates none states none, so the next one
// that does logs its skips afresh.
func (a *Agent) pruneNotes(seen map[string]bool) {
	for key := range a.notes {
		if (strings.HasPrefix(key, "member/") || strings.HasPrefix(key, "instance/")) && !seen[key] {
			delete(a.notes, key)
		}
	}
}

// note logs msg for key when it differs from the last one logged for key,
// and forgets key when msg is empty. A skip that holds for days (an
// Instance declared ahead of its join) is logged once, not every tick.
func (a *Agent) note(key, msg string) {
	if a.notes[key] == msg {
		return
	}
	if msg == "" {
		delete(a.notes, key)
		return
	}
	a.notes[key] = msg
	a.log.Print(msg)
}
