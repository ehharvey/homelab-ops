package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
)

// upgradeSim runs whole agents against one fake Incus, through the real
// incusregistry and leaderelection, to watch a self-upgrade from end to end:
// every agent instance Incus has Running gets an Agent, which ticks once per
// simulated second, and an instance the leader creates starts ticking on the
// next round, as a container that has booted does.
type upgradeSim struct {
	t      *testing.T
	world  *fakeIncus
	clock  *clock
	git    configsync.Snapshot // what every agent's next sync fetches
	agents map[string]*Agent
	events []string // "<seconds> <what>", the first time each was seen
	seen   map[string]bool

	noConfig  map[string]bool   // agents whose clone never succeeds: Current errors
	afterTick func(name string) // runs after each agent's tick, e.g. to crash one
}

func newUpgradeSim(t *testing.T, world *fakeIncus, snap configsync.Snapshot) *upgradeSim {
	c := &clock{t: t0}
	world.clock = c.now
	return &upgradeSim{t: t, world: world, clock: c, git: snap, agents: map[string]*Agent{}, seen: map[string]bool{}}
}

func (s *upgradeSim) agentFor(inst incuslocal.Instance) *Agent {
	if a, ok := s.agents[inst.Name]; ok {
		return a
	}
	gen, _ := strconv.ParseInt(inst.Config[incuslocal.GenerationTagKey], 10, 64)
	node := strings.TrimSuffix(strings.TrimPrefix(inst.Name, "agent-"), "-g"+strconv.FormatInt(gen, 10))
	sync := &fakeSync{have: map[string]bool{"c1": true, "c2": true, "c3": true}}
	sync.onSync = func(f *fakeSync) { f.snap = s.git }
	sync.snap = s.git
	sync.noSyncOK = s.noConfig[inst.Name]
	reg := &incusregistry.Registry{Client: s.world, Self: inst.Name, Node: node, Generation: gen}
	a := New(inst.Name, node, gen, sync, reg, s.world.as(inst.Name), testRepo, log.New(io.Discard, "", 0))
	a.now = s.clock.now
	a.Upgrade = Upgrade{HeartbeatStaleAfter: 5 * time.Second, MinHealthy: 10 * time.Second, HealthDeadline: 5 * time.Minute, MaxAttempts: 3}
	s.agents[inst.Name] = a
	return a
}

// round advances the clock a second and ticks every running agent, oldest
// name first, checking after each tick that at most one instance acts.
func (s *upgradeSim) round() {
	s.t.Helper()
	s.clock.t = s.clock.t.Add(time.Second)
	insts, _ := s.world.ListInstances(context.Background())
	for _, inst := range insts {
		if inst.Status != incuslocal.StatusRunning || inst.Config[incuslocal.AppTagKey] != agentApp.Name {
			continue
		}
		i := s.world.find(inst.Name)
		if i < 0 || s.world.instances[i].Status != incuslocal.StatusRunning {
			continue // retired earlier in this round
		}
		s.agentFor(inst).Tick(context.Background())
		if s.afterTick != nil {
			s.afterTick(inst.Name)
		}
		s.observe()
	}
}

// observe fails on two acting instances, and logs each state first seen.
func (s *upgradeSim) observe() {
	s.t.Helper()
	var acting []string
	for _, inst := range s.world.instances {
		if inst.Status == incuslocal.StatusRunning && inst.Config[incusregistry.KeyActing] == "true" {
			acting = append(acting, inst.Name)
		}
		if inst.Config[incusregistry.KeyDraining] == "true" {
			s.event(fmt.Sprintf("%s draining %s", inst.Name, inst.Config[incusregistry.KeyDrainReason]))
		}
		if inst.Config[KeyHealthySince] != "" {
			s.event(inst.Name + " healthy")
		}
		s.event(inst.Name + " exists")
	}
	if len(acting) > 1 {
		s.t.Fatalf("at %v: %d instances acting at once: %q (events so far: %q)", s.clock.t.Sub(t0), len(acting), acting, s.events)
	}
	if len(acting) == 1 {
		s.event(acting[0] + " acting")
	}
	for _, name := range s.world.deleted() {
		s.event(name + " deleted")
	}
}

func (s *upgradeSim) event(e string) {
	if !s.seen[e] {
		s.seen[e] = true
		s.events = append(s.events, fmt.Sprintf("%d %s", int(s.clock.t.Sub(t0).Seconds()), e))
	}
}

// at is the second e was first seen, or -1.
func (s *upgradeSim) at(e string) int {
	for _, ev := range s.events {
		sec, what, _ := strings.Cut(ev, " ")
		if what == e {
			n, _ := strconv.Atoi(sec)
			return n
		}
	}
	return -1
}

// runUntil runs rounds until done holds, failing after limit of them.
func (s *upgradeSim) runUntil(limit int, done func() bool) {
	s.t.Helper()
	for range limit {
		s.round()
		if done() {
			return
		}
	}
	s.t.Fatalf("not done after %d rounds; instances %q, events %q", limit, s.names(), s.events)
}

func (s *upgradeSim) names() []string {
	var out []string
	for _, inst := range s.world.instances {
		out = append(out, inst.Name+"("+inst.Location+")")
	}
	return out
}

// onImage reports whether every agent in the world is on ref's image and
// one of them acts, with exactly the instances want.
func (s *upgradeSim) settled(want ...string) bool {
	var got []string
	acting := 0
	for _, inst := range s.world.instances {
		if inst.Config[incuslocal.ImageTagKey] != agentdeploy.ImageTag(agentApp.Image) {
			return false
		}
		if inst.Config[incusregistry.KeyActing] == "true" {
			acting++
		}
		got = append(got, inst.Name)
	}
	slices.Sort(got)
	return acting == 1 && slices.Equal(got, want)
}

func gitAt(commit string, image bool, instances ...string) configsync.Snapshot {
	snap := fleetSnapshot(instances...)
	snap.Commit = commit
	if !image {
		snap.Config.Apps[0].Image = staleImage
	}
	return snap
}

// A standalone host's whole upgrade, on an unclustered daemon: the agent
// creates its own successor target-less, drains for it once it is
// sustained-healthy, and the successor takes over and retires it. At most one
// instance acts at every step, and no instance deletes itself.
func TestStandaloneSelfUpgradeHandsOverWithOneActingInstanceThroughout(t *testing.T) {
	world := &fakeIncus{location: incuslocal.UnclusteredLocation, instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", incuslocal.UnclusteredLocation, 0, staleImage),
	}}
	s := newUpgradeSim(t, world, gitAt("c1", false, "node0"))
	s.runUntil(5, func() bool { return s.at("agent-node0-g0 acting") >= 0 })

	s.git = gitAt("c2", true, "node0") // bump the agent App's image
	s.runUntil(120, func() bool { return s.settled("agent-node0-g1") })

	if c := world.creates; len(c) != 1 || c[0].target != "" || c[0].req.Name != "agent-node0-g1" {
		t.Errorf("creates = %+v, want exactly agent-node0-g1, target-less", c)
	}
	inOrder(t, s,
		"agent-node0-g1 exists",
		"agent-node0-g1 healthy",
		"agent-node0-g0 draining superseded",
		"agent-node0-g1 acting",
		"agent-node0-g0 deleted",
	)
	if held := s.at("agent-node0-g0 draining superseded") - s.at("agent-node0-g1 healthy"); held < 10 {
		t.Errorf("drained %ds into the candidate's healthy streak, before its MinHealthy (10s)", held)
	}
	if !slices.Contains(world.by, "agent-node0-g1: delete agent-node0-g0") {
		t.Errorf("changes by agent = %q; want the candidate to have deleted the old instance", world.by)
	}
	noSelfDeletes(t, world)
}

// A multi-member cluster: the primary creates both hosts' successors,
// targeted, retires the follower's old instance itself, and hands over on its
// own host as a standalone host does.
func TestClusterSelfUpgradeReplacesEveryHostsAgentWithOneActingInstanceThroughout(t *testing.T) {
	world := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		agentGen("agent-node1-g0", "node1", 0, staleImage),
	}}
	s := newUpgradeSim(t, world, gitAt("c1", false, "node0", "node1"))
	s.runUntil(5, func() bool { return s.at("agent-node0-g0 acting") >= 0 })

	s.git = gitAt("c2", true, "node0", "node1")
	s.runUntil(120, func() bool { return s.settled("agent-node0-g1", "agent-node1-g1") })

	targets := map[string]string{}
	for _, c := range world.creates {
		targets[c.req.Name] = c.target
	}
	if want := map[string]string{"agent-node0-g1": "node0", "agent-node1-g1": "node1"}; fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Errorf("creates (name: target) = %v, want %v", targets, want)
	}
	inOrder(t, s, "agent-node1-g1 healthy", "agent-node1-g0 deleted")
	inOrder(t, s, "agent-node0-g1 healthy", "agent-node0-g0 draining superseded", "agent-node0-g1 acting", "agent-node0-g0 deleted")
	if s.at("agent-node1-g1 acting") >= 0 || s.at("agent-node1-g0 acting") >= 0 {
		t.Errorf("a follower acted: events %q", s.events)
	}
	for _, change := range world.by {
		if strings.HasSuffix(change, "delete agent-node1-g0") && !strings.HasPrefix(change, "agent-node0-") {
			t.Errorf("%q: want the acting primary to retire the old follower", change)
		}
	}
	noSelfDeletes(t, world)
}

// A candidate whose clone never succeeds still heartbeats every tick, but
// never publishes a commit, so it could never lead. The old primary must not
// hand over to it: it keeps acting, and the candidate is deleted at its
// deadline and the attempt counted.
func TestCandidateThatCannotReadItsConfigIsNeverHandedOverTo(t *testing.T) {
	world := &fakeIncus{location: incuslocal.UnclusteredLocation, instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", incuslocal.UnclusteredLocation, 0, staleImage),
	}}
	s := newUpgradeSim(t, world, gitAt("c1", false, "node0"))
	s.noConfig = map[string]bool{"agent-node0-g1": true}
	s.runUntil(5, func() bool { return s.at("agent-node0-g0 acting") >= 0 })

	s.git = gitAt("c2", true, "node0")
	s.runUntil(400, func() bool { return s.at("agent-node0-g1 deleted") >= 0 })

	if at := s.at("agent-node0-g0 draining superseded"); at >= 0 {
		t.Errorf("the old primary drained for a candidate that couldn't lead, at %ds; events %q", at, s.events)
	}
	if s.at("agent-node0-g1 healthy") >= 0 {
		t.Errorf("a candidate that never published a commit was marked healthy; events %q", s.events)
	}
	if got := world.instances[world.find("agent-node0-g0")].Config[KeyUpgradeAttempts]; got != "1" {
		t.Errorf("upgrade-attempts = %q, want the failed candidate counted", got)
	}
	noSelfDeletes(t, world)
}

// The candidate crashes right after the old primary drained for it, before
// it takes the lead. The old primary's drain clears once the candidate isn't
// Running, so it leads again rather than leave the host with no leader, and
// then deletes the dead candidate at its deadline and tries again, with a
// new instance of the same generation, which takes over.
func TestOldPrimaryLeadsAgainWhenItsCandidateDiesBeforeTakingOver(t *testing.T) {
	world := &fakeIncus{location: incuslocal.UnclusteredLocation, instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", incuslocal.UnclusteredLocation, 0, staleImage),
	}}
	s := newUpgradeSim(t, world, gitAt("c1", false, "node0"))
	crashed := false
	s.afterTick = func(name string) {
		if crashed || name != "agent-node0-g0" {
			return
		}
		if world.instances[world.find("agent-node0-g0")].Config[incusregistry.KeyDraining] == "true" {
			crashed = true
			world.instances[world.find("agent-node0-g1")].Status = "Stopped"
		}
	}
	s.runUntil(5, func() bool { return s.at("agent-node0-g0 acting") >= 0 })

	s.git = gitAt("c2", true, "node0")
	s.runUntil(60, func() bool { return crashed })
	drainedAt := s.at("agent-node0-g0 draining superseded")
	s.runUntil(10, func() bool {
		old := world.instances[world.find("agent-node0-g0")]
		return old.Config[incusregistry.KeyDraining] == "false" && old.Config[incusregistry.KeyActing] == "true"
	})
	if s.at("agent-node0-g1 acting") >= 0 {
		t.Fatalf("the crashed candidate acted; events %q", s.events)
	}

	s.runUntil(400, func() bool { return s.settled("agent-node0-g1") })
	if drainedAt < 0 || s.at("agent-node0-g1 deleted") < drainedAt {
		t.Errorf("want the dead candidate deleted after the drain at %ds; events %q", drainedAt, s.events)
	}
	noSelfDeletes(t, world)
}

// inOrder fails unless each event was seen, no earlier than the one before.
func inOrder(t *testing.T, s *upgradeSim, events ...string) {
	t.Helper()
	prev := -1
	for _, e := range events {
		at := s.at(e)
		if at < 0 || at < prev {
			t.Errorf("event %q at %ds, want it after %ds; events %q", e, at, prev, s.events)
			return
		}
		prev = at
	}
}

func noSelfDeletes(t *testing.T, world *fakeIncus) {
	t.Helper()
	for _, change := range world.by {
		who, what, _ := strings.Cut(change, ": ")
		if what == "delete "+who || what == "stop "+who {
			t.Errorf("%q: an instance never stops or deletes itself", change)
		}
	}
}
