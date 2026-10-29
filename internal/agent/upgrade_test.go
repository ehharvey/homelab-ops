package agent

import (
	"context"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
	"github.com/ehharvey/homelab-ops/internal/leaderelection"
)

// staleImage is what the running agents were created from; agentApp.Image is
// what git now declares.
var staleImage = config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:v0.1"}

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// agentGen is generation gen of an agent on member, created at t0.
func agentGen(name, member string, gen int, image config.ImageRef) incuslocal.Instance {
	inst := agentOn(name, member, image)
	inst.Config[incuslocal.GenerationTagKey] = strconv.Itoa(gen)
	inst.CreatedAt = t0
	return inst
}

// candidate is generation 1 of an agent on member, on the declared image,
// that has published commit c1 (fleetSnapshot's), as one that has synced
// and asked MayAct has.
func candidate(name, member string) incuslocal.Instance {
	return with(agentGen(name, member, 1, agentApp.Image), incusregistry.KeyCommit, "c1")
}

// with returns inst with keys added to its config.
func with(inst incuslocal.Instance, keys ...string) incuslocal.Instance {
	for i := 0; i+1 < len(keys); i += 2 {
		inst.Config[keys[i]] = keys[i+1]
	}
	return inst
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// clock is a settable now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// upgradeLeader is agent self (generation gen, on node0) over inc, with git
// at snap, and the clock at t0. peers seeds the registry.
func upgradeLeader(self string, gen int64, snap configsync.Snapshot, inc *fakeIncus, peers map[string]leaderelection.Peer) (*Agent, *fakeReg, *clock) {
	if peers == nil {
		peers = map[string]leaderelection.Peer{}
	}
	r := &fakeReg{peers: peers, reasons: map[string]string{}, status: map[string]string{}, self: self, node: "node0", gen: gen}
	if _, ok := peers[self]; !ok {
		peers[self] = leaderelection.Peer{Node: "node0", Generation: gen}
	}
	c := &clock{t: t0}
	inc.clock = c.now
	a := New(self, "node0", gen, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, r, inc, testRepo, log.New(io.Discard, "", 0))
	a.now = c.now
	return a, r, c
}

func tick(t *testing.T, a *Agent, r *fakeReg) {
	t.Helper()
	a.Tick(context.Background())
	if !r.peers[a.self].Acting && !r.peers[a.self].Draining {
		t.Fatalf("setup: %s should lead (status %q)", a.self, r.status[a.self])
	}
}

func TestUpgradeCreatesOneCandidatePerHostTargetedOnAMultiMemberCluster(t *testing.T) {
	inc := &fakeIncus{
		members: online("node0", "node1", "node2"),
		instances: []incuslocal.Instance{
			agentGen("agent-node0-g0", "node0", 0, staleImage),
			agentGen("agent-node1-g0", "node1", 0, staleImage),
			agentGen("agent-node2-g2", "node2", 2, staleImage),
		},
	}
	a, r, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0", "node1", "node2"), inc, nil)
	a.ChildEnv = map[string]string{"AGENT_TICK_INTERVAL": "2s", "AGENT_NODE_NAME": "not-this"}
	tick(t, a, r)

	want := map[string]string{"node0": "agent-node0-g1", "node1": "agent-node1-g1", "node2": "agent-node2-g3"}
	if got := inc.targets(); !slices.Equal(got, []string{"node0", "node1", "node2"}) {
		t.Fatalf("create targets = %q, want one per member, each targeted", got)
	}
	for _, c := range inc.creates {
		gen := strings.TrimPrefix(want[c.target], "agent-"+c.target+"-g")
		for key, val := range map[string]string{
			incuslocal.AppTagKey:              "agent",
			incuslocal.GenerationTagKey:       gen,
			incuslocal.ImageTagKey:            agentdeploy.ImageTag(agentApp.Image),
			"environment.AGENT_INSTANCE_NAME": want[c.target],
			"environment.AGENT_NODE_NAME":     c.target, // Desired's own key wins over ChildEnv
			"environment.AGENT_TICK_INTERVAL": "2s",     // inherited
			"environment.CONFIG_REPO_URL":     testRepo.URL,
		} {
			if got := c.req.Config[key]; got != val {
				t.Errorf("create on %s: Config[%q] = %q, want %q", c.target, key, got, val)
			}
		}
		if c.req.Name != want[c.target] {
			t.Errorf("create on %s named %s, want %s", c.target, c.req.Name, want[c.target])
		}
	}

	// Each host now has a candidate on the declared image: no second one.
	tick(t, a, r)
	if n := len(inc.creates); n != 3 {
		t.Errorf("creates after a second tick = %d, want still 3", n)
	}
}

// A one-member cluster and an unclustered daemon are both standalone hosts
// (§30): the candidate goes target-less, which the latter requires.
func TestStandaloneHostUpgradesItselfWithoutATarget(t *testing.T) {
	for _, tt := range []struct {
		name     string
		location string
		members  []incuslocal.Member
	}{
		{"one-member cluster", "node0", online("node0")},
		{"unclustered daemon", incuslocal.UnclusteredLocation, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inc := &fakeIncus{members: tt.members, location: tt.location,
				instances: []incuslocal.Instance{agentGen("agent-node0-g0", tt.location, 0, staleImage)}}
			a, r, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
			tick(t, a, r)
			if len(inc.creates) != 1 || inc.creates[0].target != "" || inc.creates[0].req.Name != "agent-node0-g1" {
				t.Fatalf("creates = %+v, want exactly agent-node0-g1 with no target", inc.creates)
			}
			if tt.members == nil && inc.memberLists != 0 {
				t.Errorf("listed members %d times on an unclustered daemon, which has none", inc.memberLists)
			}
		})
	}
}

// Leadership needs a kind: Designation naming this node, standalone or not
// (§30 as amended by #109): there is no implicit standalone primary.
func TestNoUpgradeWithoutADesignationNamingThisNode(t *testing.T) {
	noDesignation := fleetSnapshot("node0")
	noDesignation.Config.Designations = nil
	otherPrimary := fleetSnapshot("node0", "node1")
	otherPrimary.Config.Designations = []config.Designation{{Primary: "node1"}}

	for _, tt := range []struct {
		name string
		snap configsync.Snapshot
		inc  *fakeIncus
	}{
		{"standalone host, no Designation", noDesignation, &fakeIncus{location: incuslocal.UnclusteredLocation,
			instances: []incuslocal.Instance{agentGen("agent-node0-g0", incuslocal.UnclusteredLocation, 0, staleImage)}}},
		{"non-primary member", otherPrimary, &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{
			agentGen("agent-node0-g0", "node0", 0, staleImage), agentGen("agent-node1-g0", "node1", 0, staleImage)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, r, _ := upgradeLeader("agent-node0-g0", 0, tt.snap, tt.inc, nil)
			a.Tick(context.Background())
			if r.peers[a.self].Acting {
				t.Fatalf("acting with status %q; want no leader", r.status[a.self])
			}
			if len(tt.inc.creates) != 0 || len(tt.inc.changes) != 0 {
				t.Errorf("creates = %+v, changes = %q; want none", tt.inc.creates, tt.inc.changes)
			}
		})
	}
}

// The image moved on mid-upgrade: the candidate for the old target is
// stopped, deleted and replaced with one on the latest image.
func TestImageBumpedMidUpgradeReplacesTheStaleCandidate(t *testing.T) {
	between := config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:v0.2"}
	inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		with(agentGen("agent-node0-g1", "node0", 1, between), incusregistry.KeyHeartbeat, rfc(t0)),
	}}
	a, r, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
	tick(t, a, r)

	if want := []string{"stop agent-node0-g1", "delete agent-node0-g1"}; !slices.Equal(inc.changes, want) {
		t.Errorf("changes = %q, want %q", inc.changes, want)
	}
	if len(inc.creates) != 1 || inc.creates[0].req.Config[incuslocal.ImageTagKey] != agentdeploy.ImageTag(agentApp.Image) {
		t.Fatalf("creates = %+v, want one candidate on the declared image", inc.creates)
	}
}

// A candidate on a stale image that is acting (it took over already) isn't
// this agent's to stop.
func TestActingInstanceIsNeverRetired(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, agentApp.Image),
		agentGen("agent-node1-g0", "node1", 0, staleImage),
		with(agentGen("agent-node1-g1", "node1", 1, staleImage), incusregistry.KeyActing, "true"),
	}}
	a, r, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0", "node1"), inc, nil)
	tick(t, a, r)
	if len(inc.changes) != 0 || len(inc.creates) != 0 {
		t.Errorf("changes = %q, creates = %+v; want none while another instance acts", inc.changes, inc.creates)
	}
}

func TestHealthySinceIsSetOnAFreshHeartbeatAndClearedOnAStaleOne(t *testing.T) {
	inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		with(candidate("agent-node0-g1", "node0"), incusregistry.KeyHeartbeat, rfc(t0)),
	}}
	a, r, c := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
	cand := func() map[string]string { return inc.instances[inc.find("agent-node0-g1")].Config }

	tick(t, a, r)
	if got := cand()[KeyHealthySince]; got != rfc(t0) {
		t.Fatalf("healthy-since = %q after a fresh heartbeat, want %q", got, rfc(t0))
	}

	// The candidate stops ticking: its heartbeat ages past the threshold.
	c.t = t0.Add(a.Upgrade.HeartbeatStaleAfter + time.Second)
	tick(t, a, r)
	if got := cand()[KeyHealthySince]; got != "" {
		t.Errorf("healthy-since = %q after a stale heartbeat, want it cleared", got)
	}
	if r.peers[a.self].Draining {
		t.Error("the old instance drained for a candidate whose heartbeat is stale")
	}
}

func TestOldInstanceDrainsOnlyOnceItsCandidateIsSustainedHealthy(t *testing.T) {
	inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		candidate("agent-node0-g1", "node0"),
	}}
	a, r, c := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
	// The candidate ticks on every step, as a healthy agent does.
	beat := func() { inc.instances[inc.find("agent-node0-g1")].Config[incusregistry.KeyHeartbeat] = rfc(c.t) }

	for _, at := range []time.Duration{0, time.Minute, a.Upgrade.MinHealthy - time.Second} {
		c.t = t0.Add(at)
		beat()
		tick(t, a, r)
		if r.peers[a.self].Draining {
			t.Fatalf("drained %v into the candidate's streak, before AGENT_MIN_HEALTHY_DURATION (%v)", at, a.Upgrade.MinHealthy)
		}
	}

	c.t = t0.Add(a.Upgrade.MinHealthy)
	beat()
	a.Tick(context.Background())
	self := r.peers[a.self]
	if !self.Draining || r.reasons[a.self] != incusregistry.DrainSuperseded {
		t.Fatalf("draining = %v, reason %q; want draining for %q once the candidate held healthy for %v", self.Draining, r.reasons[a.self], incusregistry.DrainSuperseded, a.Upgrade.MinHealthy)
	}
	if self.Acting {
		t.Error("still acting after draining: the candidate would wait a tick longer than it needs to")
	}
	if !strings.HasPrefix(r.status[a.self], "superseded") {
		t.Errorf("status = %q, want it to say it was superseded", r.status[a.self])
	}
	if slices.Contains(inc.deleted(), a.self) {
		t.Error("the old instance deleted itself")
	}

	// While the candidate runs, a later successful sync doesn't clear the
	// drain: the candidate is what leads now.
	r.peers["agent-node0-g1"] = leaderelection.Peer{Node: "node0", Generation: 1, Commit: "c1"}
	a.Tick(context.Background())
	if !r.peers[a.self].Draining {
		t.Error("a superseded drain was cleared while its candidate runs")
	}

	// The candidate stops before it takes the lead: the old instance clears
	// its drain and leads again, rather than leave the cluster leaderless.
	delete(r.peers, "agent-node0-g1")
	inc.instances[inc.find("agent-node0-g1")].Status = "Stopped"
	a.Tick(context.Background())
	if self := r.peers[a.self]; self.Draining || !self.Acting {
		t.Errorf("after its candidate went away: draining=%v acting=%v (status %q, reason %q); want it leading again", self.Draining, self.Acting, r.status[a.self], r.reasons[a.self])
	}
}

// A heartbeat is written every tick, whatever the tick decided, so a fresh
// one alone doesn't make a candidate healthy: one that couldn't lead once
// handed over to never replaces the old instance, however long it beats.
func TestCandidateThatCouldNotLeadIsNotHealthy(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
	}{
		{"draining", []string{incusregistry.KeyDraining, "true", incusregistry.KeyDrainReason, incusregistry.DrainSyncFailure}},
		{"never published a commit (can't read its config)", []string{incusregistry.KeyCommit, ""}},
		{"on another commit", []string{incusregistry.KeyCommit, "c0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cand := with(candidate("agent-node0-g1", "node0"), KeyHealthySince, rfc(t0))
			inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
				agentGen("agent-node0-g0", "node0", 0, staleImage),
				with(cand, tt.keys...),
			}}
			a, r, c := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
			c.t = t0.Add(a.Upgrade.MinHealthy)
			inc.instances[inc.find("agent-node0-g1")].Config[incusregistry.KeyHeartbeat] = rfc(c.t)
			tick(t, a, r)
			if r.peers[a.self].Draining {
				t.Fatal("the old instance drained for a candidate that couldn't lead")
			}
			if got := inc.instances[inc.find("agent-node0-g1")].Config[KeyHealthySince]; got != "" {
				t.Errorf("healthy-since = %q, want the streak cleared", got)
			}
		})
	}
}

// An agent created before #203 carries no generation tag; its name's -g<N>
// suffix is its generation, so it is upgraded like any other.
func TestUntaggedAgentIsUpgradedFromItsNameSuffix(t *testing.T) {
	untagged := func(inst incuslocal.Instance) incuslocal.Instance {
		delete(inst.Config, incuslocal.GenerationTagKey)
		return inst
	}
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{
		untagged(agentOn("agent-node0-g0", "node0", agentApp.Image)),
		untagged(agentOn("agent-node1-g2", "node1", staleImage)),
	}}
	a, r, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0", "node1"), inc, nil)
	tick(t, a, r)
	if len(inc.creates) != 1 || inc.creates[0].req.Name != "agent-node1-g3" || inc.creates[0].target != "node1" {
		t.Fatalf("creates = %+v, want agent-node1-g3 on node1", inc.creates)
	}
}

// An agent that sits on one member under another node's name is no
// generation of that member's agent: it is never retired for it, and never
// retires that member's own agent.
func TestAgentOnAnotherMemberIsLeftAlone(t *testing.T) {
	for _, tt := range []struct {
		name      string
		self      string
		instances []incuslocal.Instance
		keep      string
	}{
		{"beside that member's up-to-date agent", "agent-node0-g0", []incuslocal.Instance{
			agentGen("agent-node0-g0", "node0", 0, agentApp.Image),
			agentGen("agent-node1-g0", "node1", 0, agentApp.Image),
			agentGen("agent-node2-g0", "node1", 0, staleImage), // node2's, on node1
		}, "agent-node2-g0"},
		{"leading from beside that member's agent", "agent-node0-g0", []incuslocal.Instance{
			agentGen("agent-node0-g0", "node1", 0, agentApp.Image), // the leader, on node1
			agentGen("agent-node1-g0", "node1", 0, agentApp.Image),
		}, "agent-node1-g0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inc := &fakeIncus{members: online("node0", "node1"), instances: tt.instances}
			a, r, _ := upgradeLeader(tt.self, 0, fleetSnapshot("node0", "node1"), inc, nil)
			tick(t, a, r)
			for _, ch := range inc.changes {
				if strings.HasSuffix(ch, " "+tt.keep) {
					t.Errorf("changes = %q, want %s left alone", inc.changes, tt.keep)
				}
			}
		})
	}
}

// A hold found earlier in the pass in which the leader hands over still
// reaches its status.
func TestHoldsSurviveTheLeadersHandover(t *testing.T) {
	inc := &fakeIncus{members: online("host1", "node0"), instances: []incuslocal.Instance{
		with(agentGen("agent-host1-g0", "host1", 0, staleImage),
			KeyUpgradeAttempts, "3", KeyUpgradeImage, agentdeploy.ImageTag(agentApp.Image)),
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		with(candidate("agent-node0-g1", "node0"), KeyHealthySince, rfc(t0)),
	}}
	a, r, c := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("host1", "node0"), inc, nil)
	c.t = t0.Add(a.Upgrade.MinHealthy)
	inc.instances[inc.find("agent-node0-g1")].Config[incusregistry.KeyHeartbeat] = rfc(c.t)
	a.Tick(context.Background())
	if st := r.status[a.self]; !strings.HasPrefix(st, "superseded") || !strings.Contains(st, "upgrade of agent-host1-g0 held") {
		t.Errorf("status = %q, want superseded and host1's hold", st)
	}
}

func TestCandidatePastItsDeadlineIsDeletedAndCountedUpToTheCap(t *testing.T) {
	inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		agentGen("agent-node0-g1", "node0", 1, agentApp.Image), // never heartbeats
	}}
	a, r, c := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
	old := func() map[string]string { return inc.instances[inc.find("agent-node0-g0")].Config }
	want := agentdeploy.ImageTag(agentApp.Image)

	c.t = t0.Add(a.Upgrade.HealthDeadline - time.Second)
	tick(t, a, r)
	if len(inc.deleted()) != 0 {
		t.Fatalf("deleted %q before the deadline", inc.deleted())
	}

	for n := 1; n <= a.Upgrade.MaxAttempts; n++ {
		c.t = c.t.Add(a.Upgrade.HealthDeadline + time.Second)
		tick(t, a, r) // the candidate is past its deadline: deleted, and counted
		if got := old()[KeyUpgradeAttempts]; got != strconv.Itoa(n) || old()[KeyUpgradeImage] != want {
			t.Fatalf("attempt %d: upgrade-attempts = %q, upgrade-image = %q; want %d and %s", n, got, old()[KeyUpgradeImage], n, want)
		}
		tick(t, a, r) // and retried, until the cap
		if wantCreates := min(n, a.Upgrade.MaxAttempts-1); len(inc.creates) != wantCreates {
			t.Fatalf("after attempt %d: %d creates, want %d", n, len(inc.creates), wantCreates)
		}
	}
	if !strings.Contains(r.status[a.self], "held") {
		t.Errorf("status = %q, want it to say the upgrade is held at the cap", r.status[a.self])
	}

	// A different image in git resets the count.
	snap := fleetSnapshot("node0")
	snap.Config.Apps[0].Image.Alias = "ehharvey/homelab-ops/agent:v0.4"
	a.sync.(*fakeSync).snap = snap
	tick(t, a, r)
	if n := len(inc.creates); n != a.Upgrade.MaxAttempts {
		t.Errorf("creates after a new image = %d, want one more (%d)", n, a.Upgrade.MaxAttempts)
	}
}

// On the primary's host, the candidate takes over once the old instance has
// drained and published acting: false, and then retires it.
func TestCandidateRetiresTheOldInstanceOnceItLeads(t *testing.T) {
	inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
		with(agentGen("agent-node0-g0", "node0", 0, staleImage), incusregistry.KeyDraining, "true", incusregistry.KeyDrainReason, incusregistry.DrainSuperseded),
		agentGen("agent-node0-g1", "node0", 1, agentApp.Image),
	}}
	peers := map[string]leaderelection.Peer{"agent-node0-g0": {Node: "node0", Generation: 0, Draining: true}}
	a, r, _ := upgradeLeader("agent-node0-g1", 1, fleetSnapshot("node0"), inc, peers)
	tick(t, a, r)
	if want := []string{"stop agent-node0-g0", "delete agent-node0-g0"}; !slices.Equal(inc.changes, want) {
		t.Errorf("changes = %q, want the candidate to retire the old instance: %q", inc.changes, want)
	}
	if len(inc.creates) != 0 {
		t.Errorf("creates = %+v, want none: the leader is on the declared image", inc.creates)
	}
}

// On a follower's host, the acting primary retires the old instance once the
// follower's candidate is sustained-healthy. A follower never acts, so it
// can't retire its own.
func TestPrimaryRetiresAnOldFollowerOnceItsCandidateIsSustainedHealthy(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, agentApp.Image),
		agentGen("agent-node1-g0", "node1", 0, staleImage),
		with(candidate("agent-node1-g1", "node1"), KeyHealthySince, rfc(t0)),
	}}
	a, r, c := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0", "node1"), inc, nil)
	c.t = t0.Add(a.Upgrade.MinHealthy)
	inc.instances[inc.find("agent-node1-g1")].Config[incusregistry.KeyHeartbeat] = rfc(c.t)
	tick(t, a, r)
	if want := []string{"stop agent-node1-g0", "delete agent-node1-g0"}; !slices.Equal(inc.changes, want) {
		t.Errorf("changes = %q, want %q", inc.changes, want)
	}
}

// retire is the one path to a stop or delete; it refuses this agent's own
// instance and an acting one, whatever its caller decided.
func TestRetireRefusesItselfAndAnActingInstance(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		with(agentGen("agent-node1-g0", "node1", 0, staleImage), incusregistry.KeyActing, "true"),
	}}
	a, _, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0", "node1"), inc, nil)
	for _, inst := range inc.instances {
		if a.retire(context.Background(), agentInstance{Instance: inst}, "test") {
			t.Errorf("retired %s", inst.Name)
		}
	}
	if len(inc.changes) != 0 {
		t.Errorf("changes = %q, want none", inc.changes)
	}
}
