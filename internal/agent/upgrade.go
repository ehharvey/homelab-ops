package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
)

// The self-upgrade's bookkeeping keys (#109). The acting primary writes them,
// on other agents' instances as well as its own: each key has that one
// writer, since at most one agent acts at a time, and a PATCH leaves the
// instance's own user.homelab-ops.agent.* keys alone (incuslocal.SetUserKeys).
const (
	// KeyHealthySince is on a candidate: the RFC 3339 UTC time of the first
	// tick it was seen healthy (unhealthy), in the current streak. Any tick
	// that sees it unhealthy clears it.
	KeyHealthySince = "user.homelab-ops.healthy-since"
	// KeyUpgradeAttempts is on the instance a candidate would replace: how
	// many candidates for KeyUpgradeImage were deleted for missing their
	// health deadline.
	KeyUpgradeAttempts = "user.homelab-ops.upgrade-attempts"
	// KeyUpgradeImage is the ImageTag those attempts were for. When git
	// names a different image, the count starts again.
	KeyUpgradeImage = "user.homelab-ops.upgrade-image"
)

// Upgrade tunes the self-upgrade. cmd/agent reads each from the environment
// variable named beside it.
type Upgrade struct {
	// HeartbeatStaleAfter (AGENT_HEARTBEAT_STALE_AFTER): a candidate is
	// healthy while its own incusregistry.KeyHeartbeat is younger than this.
	HeartbeatStaleAfter time.Duration
	// MinHealthy (AGENT_MIN_HEALTHY_DURATION): how long a candidate stays
	// healthy, without a break, before it replaces the old instance.
	MinHealthy time.Duration
	// HealthDeadline (AGENT_UPGRADE_HEALTH_DEADLINE): a candidate not
	// sustained-healthy this long after Incus created it is deleted, and the
	// attempt counted.
	HealthDeadline time.Duration
	// MaxAttempts (AGENT_UPGRADE_MAX_ATTEMPTS): after this many counted
	// attempts at one image, no more candidates are created for it.
	MaxAttempts int
}

// DefaultUpgrade is the operator's 2026-10-27 defaults (#109).
func DefaultUpgrade() Upgrade {
	return Upgrade{
		HeartbeatStaleAfter: 90 * time.Second,
		MinHealthy:          2 * time.Minute,
		HealthDeadline:      10 * time.Minute,
		MaxAttempts:         3,
	}
}

// upgradeHost is one host whose agent the upgrade pass looks after.
type upgradeHost struct {
	node     string // its Instance name in git config
	location string // where its agents' instances sit (Instance.Location)
	target   string // the create's target: the member, on a multi-member cluster only
}

// agentInstance is one of a host's agent instances, with its generation.
type agentInstance struct {
	incuslocal.Instance
	gen int64
}

func (i agentInstance) image() string { return i.Config[incuslocal.ImageTagKey] }

// acting is whether the instance may be doing leader work: it's Running and
// has published acting: true. A stopped instance's leftover flag means
// nothing, as for incusregistry.Peers.
func (i agentInstance) acting() bool {
	return i.Status == incuslocal.StatusRunning && i.Config[incusregistry.KeyActing] == "true"
}

// upgradeAgents is the agent-only slice of #98's image-change handling
// (docs/Decisions.md §30, #109). For every host of this agent's cluster, a
// standalone host included, it replaces an agent whose image tag isn't the
// declared agent App's with generation g+1 on that image, blue-green:
//
//   - The primary creates every host's candidate, its own included, and
//     never moves for an upgrade: the designation names a node.
//   - At most one candidate per host. One the image moved on from, while
//     still a candidate, is deleted and replaced.
//   - A candidate is healthy while its own heartbeat is fresh and it could
//     lead: not draining, and at the decided commit (unhealthy). The primary
//     marks the start of the streak on it (KeyHealthySince) and clears it on
//     any unhealthy tick. Once it has held MinHealthy, the candidate replaces
//     the old instance. One that hasn't within HealthDeadline of its
//     creation is deleted, and the attempt counted on the old instance, up
//     to MaxAttempts per image.
//   - On a follower's host, the acting primary retires the old instance. On
//     the primary's own host, the old instance (this one) drains with
//     DrainSuperseded instead, the candidate takes over through §25's
//     handoff, and the candidate, now leading, retires it. No instance
//     ever deletes itself, nor one that is acting.
//
// It reports drained when this instance just drained for its candidate:
// Tick then stops leading at once. holds are the hosts whose upgrade is
// held at the attempt cap, for this agent's status key.
//
// Like ensureAgents it is leader-only, and every step is an Incus API call
// through this agent's own member (§25). Every failure is logged and retried
// next tick. ctx is the process's lifetime, pctx the tick's passTimeout under
// it.
func (a *Agent) upgradeAgents(ctx, pctx context.Context, v leaderView) (drained bool, holds []string) {
	app := v.app
	declared := map[string]config.Instance{}
	for _, inst := range v.snap.Config.Instances {
		declared[inst.Name] = inst
	}

	seen := map[string]bool{}
	for _, h := range a.upgradeHosts(v) {
		key := "upgrade/" + h.node
		seen[key] = true
		inst, ok := declared[h.node]
		if !ok {
			// A member that isn't declared is ensureAgents' to note. A
			// standalone host whose own node isn't declared would lead
			// with nothing to upgrade from.
			a.note(key, "")
			continue
		}
		if _, busy := a.creating[h.node]; busy {
			continue // a create there is still running; settleCreates watches it
		}
		var mine []agentInstance
		var bad []string
		for _, in := range v.instances {
			// h's agents sit on its member and are named for its node. One
			// that sits there under another node's name (agent-node1-g0 on
			// node2, which ensureAgents leaves alone) is neither: no
			// generation of h's, and never retired from here.
			if in.Config[incuslocal.AppTagKey] != app.Name || in.Location != h.location || !agentdeploy.IsAgentInstance(app, inst, in.Name) {
				continue
			}
			gen, ok := generation(app, inst, in)
			if !ok {
				bad = append(bad, in.Name)
				continue
			}
			mine = append(mine, agentInstance{Instance: in, gen: gen})
		}
		if len(bad) > 0 {
			a.note(key, fmt.Sprintf("upgrade: %s on %s carry no readable %s; not upgrading %s", strings.Join(bad, ", "), h.node, incuslocal.GenerationTagKey, h.node))
			continue
		}
		if len(mine) == 0 {
			a.note(key, "") // ensureAgents creates its first
			continue
		}
		slices.SortFunc(mine, func(x, y agentInstance) int {
			return cmp.Or(cmp.Compare(x.gen, y.gen), strings.Compare(x.Name, y.Name))
		})

		if ctx.Err() != nil {
			return false, holds // shutting down: there is no next tick to retry on
		}
		if pctx.Err() != nil {
			a.log.Printf("upgrade: this pass's %v is spent; the remaining hosts wait for next tick", a.passTimeout)
			return false, holds
		}
		d, hold := a.upgradeHost(pctx, key, v.snap.Commit, app, inst, h, mine)
		if d {
			return true, holds
		}
		if hold != "" {
			holds = append(holds, hold)
		}
	}
	for key := range a.notes {
		if strings.HasPrefix(key, "upgrade/") && !seen[key] {
			delete(a.notes, key)
		}
	}
	return false, holds
}

// upgradeHosts lists the hosts of this agent's cluster: on an unclustered
// daemon the one standalone host, this agent's own node; on a clustered one
// each Online member. A target is sent only on a multi-member cluster: an
// unclustered daemon rejects one, and a one-member cluster's only member is
// where Incus places the instance anyway.
func (a *Agent) upgradeHosts(v leaderView) []upgradeHost {
	if !v.clustered() {
		return []upgradeHost{{node: a.node, location: v.own.Location}}
	}
	members := slices.Clone(v.members)
	slices.SortFunc(members, func(x, y incuslocal.Member) int { return strings.Compare(x.Name, y.Name) })
	var hosts []upgradeHost
	for _, m := range members {
		if m.Status != incuslocal.MemberOnline {
			continue // ensureAgents notes it
		}
		h := upgradeHost{node: m.Name, location: m.Name}
		if len(members) > 1 {
			h.target = m.Name
		}
		hosts = append(hosts, h)
	}
	return hosts
}

// generation is in's blue-green generation: its GenerationTagKey, or, on an
// agent created before #203 began writing that tag, its -g<N> name suffix,
// which IsAgentInstance has already checked. A tag that is set but isn't a
// generation is not read past.
func generation(app config.App, inst config.Instance, in incuslocal.Instance) (int64, bool) {
	tag, tagged := in.Config[incuslocal.GenerationTagKey]
	if !tagged {
		return agentdeploy.Generation(app, inst, in.Name)
	}
	gen, err := strconv.ParseInt(tag, 10, 64)
	return gen, err == nil && gen >= 0
}

// upgradeHost takes one host's agents, oldest generation first, one step
// toward a single instance on the declared image.
func (a *Agent) upgradeHost(ctx context.Context, key, commit string, app config.App, inst config.Instance, h upgradeHost, mine []agentInstance) (drained bool, hold string) {
	want := agentdeploy.ImageTag(app.Image)
	now := a.now()

	if newest := mine[len(mine)-1]; newest.Name == a.self && len(mine) > 1 {
		// This agent leads from the newest generation on its host, which
		// rule 4 allows only once every older one there is draining or gone:
		// they have handed over to it, so it retires them.
		for _, o := range mine[:len(mine)-1] {
			a.retire(ctx, o, "superseded by "+a.self)
		}
		a.note(key, "")
		return false, ""
	}

	// At most one candidate: drop any that's on an image git no longer
	// names, isn't needed (the oldest is already on the declared image), or
	// isn't the newest.
	oldest := mine[0]
	kept := mine[:1]
	for j, c := range mine[1:] {
		last := j == len(mine)-2
		if c.Name != a.self && !c.acting() && (c.image() != want || oldest.image() == want || !last) {
			if a.retire(ctx, c, "a candidate for an image git no longer names") {
				continue
			}
		}
		kept = append(kept, c)
	}
	mine = kept

	if len(mine) == 1 {
		return false, a.startUpgrade(ctx, key, app, inst, h, oldest, want)
	}
	cand := mine[len(mine)-1]
	switch {
	case len(mine) > 2 || cand.acting() || cand.Name == a.self:
		// Left over from a step that failed, or another agent acting: leave
		// it for a later tick rather than guess.
		a.note(key, fmt.Sprintf("upgrade: %s has %d agent instances (newest %s, acting=%v); waiting for them to settle", h.node, len(mine), cand.Name, cand.acting()))
		return false, ""
	case cand.image() != want:
		return false, "" // couldn't be retired above; logged there, retried next tick
	}

	if a.sustainedHealthy(ctx, cand, commit, now) {
		switch {
		case oldest.Name == a.self:
			if err := a.reg.SetDraining(ctx, true, incusregistry.DrainSuperseded); err != nil {
				a.log.Printf("upgrade: drain for %s: %v (retrying next tick)", cand.Name, err)
				return false, ""
			}
			a.draining, a.drainReason = true, incusregistry.DrainSuperseded
			a.log.Printf("draining: candidate %s has been healthy for %v; handing over to it", cand.Name, a.Upgrade.MinHealthy)
			a.note(key, "")
			return true, ""
		case oldest.acting():
			a.note(key, fmt.Sprintf("upgrade: %s is acting; waiting for it to stop before retiring it", oldest.Name))
		default:
			a.retire(ctx, oldest, "replaced by sustained-healthy "+cand.Name)
		}
		return false, ""
	}

	if !cand.CreatedAt.IsZero() && now.Sub(cand.CreatedAt) >= a.Upgrade.HealthDeadline {
		if !a.retire(ctx, cand, fmt.Sprintf("not sustained-healthy within %v of its creation", a.Upgrade.HealthDeadline)) {
			return false, ""
		}
		n := attempts(oldest, want) + 1
		err := a.incus.SetUserKeys(ctx, oldest.Name, map[string]string{
			KeyUpgradeAttempts: strconv.Itoa(n),
			KeyUpgradeImage:    want,
		})
		if err != nil {
			a.log.Printf("upgrade: count attempt %d on %s: %v", n, oldest.Name, err)
		} else {
			a.log.Printf("upgrade: attempt %d of %d at upgrading %s failed", n, a.Upgrade.MaxAttempts, oldest.Name)
		}
		return false, ""
	}
	a.note(key, fmt.Sprintf("upgrade: waiting for candidate %s to be healthy for %v", cand.Name, a.Upgrade.MinHealthy))
	return false, ""
}

// startUpgrade creates the candidate for a host whose one agent, old, is on
// another image than want, unless the attempts at want are spent. Its hold
// is the reason it won't, for this agent's status.
func (a *Agent) startUpgrade(ctx context.Context, key string, app config.App, inst config.Instance, h upgradeHost, old agentInstance, want string) (hold string) {
	if old.image() == want {
		a.note(key, "")
		if old.Config[KeyUpgradeImage] != "" || old.Config[KeyUpgradeAttempts] != "" {
			// Upgraded, or git went back to this image: the count is stale.
			if err := a.incus.SetUserKeys(ctx, old.Name, map[string]string{KeyUpgradeAttempts: "", KeyUpgradeImage: ""}); err != nil {
				a.log.Printf("upgrade: clear the attempt count on %s: %v", old.Name, err)
			}
		}
		return ""
	}
	if old.Status != incuslocal.StatusRunning {
		a.note(key, fmt.Sprintf("upgrade: %s on %s is %s, not %s; not upgrading it (start it, or delete it for the primary to recreate)", old.Name, h.node, old.Status, incuslocal.StatusRunning))
		return ""
	}
	if n := attempts(old, want); n >= a.Upgrade.MaxAttempts {
		hold = fmt.Sprintf("upgrade of %s held: %d candidates in a row for image %s weren't healthy for %v within %v of creation (AGENT_UPGRADE_MAX_ATTEMPTS=%d); a different image in git resets the count",
			old.Name, n, imageLabel(app.Image), a.Upgrade.MinHealthy, a.Upgrade.HealthDeadline, a.Upgrade.MaxAttempts)
		a.note(key, "upgrade: "+hold)
		return hold
	}
	a.note(key, "")

	req := a.desired(app, inst, old.gen+1)
	err := a.incus.CreateInstance(ctx, req, h.target)
	var pending *incuslocal.OperationPendingError
	switch {
	case errors.As(err, &pending):
		a.creating[h.node] = pending.ID
		a.log.Printf("upgrade: create %s on %s is still running (operation %s); checking on it next tick", req.Name, h.node, pending.ID)
	case err != nil:
		a.log.Printf("upgrade: create %s on %s: %v (retrying next tick)", req.Name, h.node, err)
	default:
		a.log.Printf("upgrade: created candidate %s on %s for image %s, replacing %s", req.Name, h.node, imageLabel(app.Image), old.Name)
	}
	return ""
}

// sustainedHealthy keeps cand's KeyHealthySince streak and reports whether
// it has held for MinHealthy. A failed write counts as not healthy yet.
func (a *Agent) sustainedHealthy(ctx context.Context, cand agentInstance, commit string, now time.Time) bool {
	why := a.unhealthy(cand, commit, now)
	raw := cand.Config[KeyHealthySince]
	since, err := time.Parse(time.RFC3339, raw)
	switch {
	case why == "" && err != nil:
		if err := a.incus.SetUserKeys(ctx, cand.Name, map[string]string{KeyHealthySince: now.UTC().Format(time.RFC3339)}); err != nil {
			a.log.Printf("upgrade: mark %s healthy: %v (retrying next tick)", cand.Name, err)
			return false
		}
		a.log.Printf("upgrade: candidate %s is healthy; its %v starts now", cand.Name, a.Upgrade.MinHealthy)
		return a.Upgrade.MinHealthy <= 0
	case why != "" && raw != "":
		if err := a.incus.SetUserKeys(ctx, cand.Name, map[string]string{KeyHealthySince: ""}); err != nil {
			a.log.Printf("upgrade: clear %s's healthy streak: %v (retrying next tick)", cand.Name, err)
		} else {
			a.log.Printf("upgrade: candidate %s is not healthy (%s); its healthy streak starts over", cand.Name, why)
		}
		return false
	case why != "":
		return false
	}
	return now.Sub(since) >= a.Upgrade.MinHealthy
}

// unhealthy is why cand isn't healthy at now, or "" if it is. A fresh
// heartbeat alone only says its process ticks: every tick writes one,
// whatever it decided. So it must also be able to lead once handed over to:
// not draining, and published at commit, the one this leader decided on.
// That publication comes from MayAct, which only gets that far once the
// candidate has read its config, so one that can't sync never passes.
func (a *Agent) unhealthy(cand agentInstance, commit string, now time.Time) string {
	hb, err := time.Parse(time.RFC3339, cand.Config[incusregistry.KeyHeartbeat])
	switch {
	case cand.Status != incuslocal.StatusRunning:
		return "it is " + cand.Status
	case err != nil || now.Sub(hb) >= a.Upgrade.HeartbeatStaleAfter:
		return "its heartbeat is stale"
	case cand.Config[incusregistry.KeyDraining] == "true":
		return "it is draining"
	case cand.Config[incusregistry.KeyCommit] != commit:
		return fmt.Sprintf("it has published commit %.12q, not the decided %.12s", cand.Config[incusregistry.KeyCommit], commit)
	}
	return ""
}

// retire stops and deletes inst, and reports whether it's gone. It never
// touches this agent's own instance or one that is acting: an instance never
// deletes itself, and stopping a leader mid-action is what the acting
// handoff exists to prevent. A forced stop is safe on everything else.
func (a *Agent) retire(ctx context.Context, inst agentInstance, why string) bool {
	if inst.Name == a.self || inst.acting() {
		return false
	}
	if inst.Status == incuslocal.StatusRunning {
		if err := a.incus.StopInstance(ctx, inst.Name); err != nil {
			a.log.Printf("upgrade: stop %s (%s): %v (retrying next tick)", inst.Name, why, err)
			return false
		}
	}
	if err := a.incus.DeleteInstance(ctx, inst.Name); err != nil {
		a.log.Printf("upgrade: delete %s (%s): %v (retrying next tick)", inst.Name, why, err)
		return false
	}
	a.log.Printf("upgrade: retired %s: %s", inst.Name, why)
	return true
}

// attempts is how many candidates for want have failed on old's host.
func attempts(old agentInstance, want string) int {
	if old.Config[KeyUpgradeImage] != want {
		return 0
	}
	n, err := strconv.Atoi(old.Config[KeyUpgradeAttempts])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// desired is agentdeploy.Desired for this agent's repo, with ChildEnv.
func (a *Agent) desired(app config.App, inst config.Instance, generation int64) lxcapi.InstancesPost {
	req := agentdeploy.Desired(app, inst, a.repo, generation)
	for name, v := range a.ChildEnv {
		if key := "environment." + name; req.Config[key] == "" {
			req.Config[key] = v
		}
	}
	return req
}

// imageLabel is a short name for ref in logs and status.
func imageLabel(ref config.ImageRef) string {
	if ref.Alias != "" {
		return ref.Alias
	}
	if len(ref.Fingerprint) > 12 {
		return ref.Fingerprint[:12]
	}
	return ref.Fingerprint
}
