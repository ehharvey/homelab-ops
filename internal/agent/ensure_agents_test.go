package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/leaderelection"
)

var testRepo = agentdeploy.Repo{URL: "https://example.test/fleet.git", Ref: "prod"}

// createCall is one CreateInstance the fake saw.
type createCall struct {
	req         lxcapi.InstancesPost
	target      string
	hasDeadline bool
}

// fakeIncus is a cluster as the leader's own member sees it. A successful
// create adds the instance on its target, as Incus would; an instance name
// already in use fails, as Incus's cluster-wide names make it.
type fakeIncus struct {
	members     []incuslocal.Member
	instances   []incuslocal.Instance
	creates     []createCall
	memberLists int
	failCreate  error // every create fails with this, when set
	// pendingCreate makes a create's wait end with it still running, as a
	// slow image pull does: the instance isn't listed, and its operation,
	// "op-<target>", stays Running in ops until a test moves it on.
	pendingCreate bool
	// stallCreate makes a create block until its ctx ends (or a second
	// passes), then report itself pending, as a create whose pull outlasts the pass does.
	stallCreate bool
	ops         map[string]*lxcapi.Operation // what Operation serves; absent is ErrOperationGone
	onCreate    func()                       // run at the start of each create, when set
}

func (f *fakeIncus) Operation(_ context.Context, id string) (*lxcapi.Operation, error) {
	op, ok := f.ops[id]
	if !ok {
		return nil, incuslocal.ErrOperationGone
	}
	return op, nil
}

func (f *fakeIncus) ListMembers(context.Context) ([]incuslocal.Member, error) {
	f.memberLists++
	return slices.Clone(f.members), nil
}

func (f *fakeIncus) ListInstances(context.Context) ([]incuslocal.Instance, error) {
	return slices.Clone(f.instances), nil
}

func (f *fakeIncus) CreateInstance(ctx context.Context, req lxcapi.InstancesPost, target string) error {
	_, hasDeadline := ctx.Deadline()
	f.creates = append(f.creates, createCall{req: req, target: target, hasDeadline: hasDeadline})
	if f.onCreate != nil {
		f.onCreate()
	}
	if f.failCreate != nil {
		return f.failCreate
	}
	if f.stallCreate {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second): // so a missing deadline fails rather than hangs
		}
	}
	if f.pendingCreate || f.stallCreate {
		id := "op-" + target
		if f.ops == nil {
			f.ops = map[string]*lxcapi.Operation{}
		}
		f.ops[id] = &lxcapi.Operation{ID: id, Status: "Running", StatusCode: lxcapi.Running}
		return fmt.Errorf("create instance %q: %w", req.Name, &incuslocal.OperationPendingError{ID: id, Err: errors.New("wait ended with it Running")})
	}
	for _, inst := range f.instances {
		if inst.Name == req.Name {
			return fmt.Errorf("create instance %q: Instance %q already exists", req.Name, req.Name)
		}
	}
	f.instances = append(f.instances, incuslocal.Instance{Name: req.Name, Status: "Running", Location: target, Config: req.Config})
	return nil
}

func (f *fakeIncus) targets() []string {
	var out []string
	for _, c := range f.creates {
		out = append(out, c.target)
	}
	return out
}

var agentApp = config.App{
	Name:     "agent",
	Type:     agentdeploy.AppType,
	Replicas: config.Replicas{PerNode: true},
	Image:    config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:latest"},
}

// fleetSnapshot declares instances and the agent App, with node0 primary.
func fleetSnapshot(instances ...string) configsync.Snapshot {
	snap := snapshot("node0", "c1")
	snap.Config.Apps = []config.App{agentApp}
	for _, n := range instances {
		snap.Config.Instances = append(snap.Config.Instances, config.Instance{Name: n})
	}
	return snap
}

func online(names ...string) []incuslocal.Member {
	var out []incuslocal.Member
	for _, n := range names {
		out = append(out, incuslocal.Member{Name: n, Status: incuslocal.MemberOnline})
	}
	return out
}

func agentOn(name, member string, image config.ImageRef) incuslocal.Instance {
	return incuslocal.Instance{Name: name, Status: "Running", Location: member, Config: map[string]string{
		incuslocal.AppTagKey:        agentApp.Name,
		incuslocal.GenerationTagKey: "0",
		incuslocal.ImageTagKey:      agentdeploy.ImageTag(image),
	}}
}

// self is the leader's own agent, on node0.
func self() incuslocal.Instance { return agentOn("agent-node0-g0", "node0", agentApp.Image) }

func leaderTick(t *testing.T, snap configsync.Snapshot, inc *fakeIncus) *Agent {
	t.Helper()
	a, r := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	a.Tick(context.Background())
	if !r.peers[a.self].Acting {
		t.Fatalf("setup: the designated primary should lead (status %q)", r.status[a.self])
	}
	return a
}

func TestLeaderCreatesOneAgentOnEachDeclaredOnlineMemberWithout(t *testing.T) {
	inc := &fakeIncus{
		members: online("node2", "node0", "node1"),
		instances: []incuslocal.Instance{
			self(),
			// Another App's instance on node2 is not node2's agent.
			{Name: "web-g0", Location: "node2", Config: map[string]string{incuslocal.AppTagKey: "web"}},
		},
	}
	a := leaderTick(t, fleetSnapshot("node0", "node1", "node2"), inc)

	if got := inc.targets(); !slices.Equal(got, []string{"node1", "node2"}) {
		t.Fatalf("create targets = %q, want exactly [node1 node2]", got)
	}
	for _, c := range inc.creates {
		inst := config.Instance{Name: c.target}
		want := agentdeploy.Desired(agentApp, inst, testRepo)
		if c.req.Name != want.Name || c.req.Name != "agent-"+c.target+"-g0" {
			t.Errorf("create on %s named %q, want %q", c.target, c.req.Name, want.Name)
		}
		for key, val := range map[string]string{
			incuslocal.AppTagKey:          "agent", // the App name, shared by every member's agent
			incuslocal.GenerationTagKey:   "0",
			incuslocal.ImageTagKey:        `{"server":"https://ghcr.io","protocol":"oci","alias":"ehharvey/homelab-ops/agent:latest"}`,
			"environment.AGENT_NODE_NAME": c.target,
			"environment.CONFIG_REPO_URL": testRepo.URL,
			"environment.CONFIG_REPO_REF": testRepo.Ref,
		} {
			if got := c.req.Config[key]; got != val {
				t.Errorf("create on %s: Config[%q] = %q, want %q", c.target, key, got, val)
			}
		}
		if !c.req.Start {
			t.Errorf("create on %s does not start the agent", c.target)
		}
	}

	// Next tick: every member has its agent, so nothing more is created.
	a.Tick(context.Background())
	if n := len(inc.creates); n != 2 {
		t.Errorf("creates after a second tick = %d, want still 2", n)
	}
}

// No image-change handling: an agent on its member is left alone, whatever
// image it runs and whatever generation it is (upgrades are #109's).
func TestMemberWithAnAgentIsUntouchedWhateverItsImage(t *testing.T) {
	old := config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:v0.1"}
	inc := &fakeIncus{
		members:   online("node0", "node1"),
		instances: []incuslocal.Instance{self(), agentOn("agent-node1-g3", "node1", old)},
	}
	leaderTick(t, fleetSnapshot("node0", "node1"), inc)
	if len(inc.creates) != 0 {
		t.Errorf("creates = %+v, want none", inc.creates)
	}
}

// Instance names are cluster-wide: agent-node1-g0 sitting on node2 is node2's
// agent, not node1's. node1 still gets a create, which fails on the name
// conflict and is retried (and logged) every tick, since nothing is torn down.
func TestAgentOnAnotherMemberIsNotThatMembersAgent(t *testing.T) {
	inc := &fakeIncus{
		members:   online("node0", "node1", "node2"),
		instances: []incuslocal.Instance{self(), agentOn("agent-node1-g0", "node2", agentApp.Image)},
	}
	a := leaderTick(t, fleetSnapshot("node0", "node1", "node2"), inc)
	if got := inc.targets(); !slices.Equal(got, []string{"node1"}) {
		t.Fatalf("create targets = %q, want exactly [node1]", got)
	}
	a.Tick(context.Background())
	if got := inc.targets(); !slices.Equal(got, []string{"node1", "node1"}) {
		t.Errorf("create targets after a second tick = %q, want node1 retried", got)
	}
	if len(inc.instances) != 2 {
		t.Errorf("instances = %+v, want the misplaced agent left in place and nothing added", inc.instances)
	}
}

func TestSkippedMembersAndInstancesGetNoCreate(t *testing.T) {
	inc := &fakeIncus{
		members: []incuslocal.Member{
			{Name: "node0", Status: incuslocal.MemberOnline},
			{Name: "stranger", Status: incuslocal.MemberOnline}, // a member no Instance declares
			{Name: "node1", Status: "Offline"},
			{Name: "node2", Status: "Evacuated"},
		},
		instances: []incuslocal.Instance{self()},
	}
	// node3 is declared but hasn't joined yet.
	a := leaderTick(t, fleetSnapshot("node0", "node1", "node2", "node3"), inc)
	if len(inc.creates) != 0 {
		t.Fatalf("creates = %+v, want none", inc.creates)
	}

	// Retried next tick: once node1 is back Online, it gets its agent.
	inc.members[2].Status = incuslocal.MemberOnline
	a.Tick(context.Background())
	if got := inc.targets(); !slices.Equal(got, []string{"node1"}) {
		t.Errorf("create targets once node1 is Online = %q, want [node1]", got)
	}
}

func TestFailedCreateIsRetriedNextTick(t *testing.T) {
	inc := &fakeIncus{
		members:    online("node0", "node1"),
		instances:  []incuslocal.Instance{self()},
		failCreate: errors.New("Failed getting remote image info"),
	}
	a := leaderTick(t, fleetSnapshot("node0", "node1"), inc)
	inc.failCreate = nil
	a.Tick(context.Background())
	a.Tick(context.Background())
	if got := inc.targets(); !slices.Equal(got, []string{"node1", "node1"}) {
		t.Errorf("create targets = %q, want one failed and one successful create on node1", got)
	}
}

func TestNonPrimaryCreatesNothing(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{self()}}
	snap := fleetSnapshot("node0", "node1")
	snap.Config.Designations = []config.Designation{{Primary: "node1"}}
	a, r := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{"c1": true}}, inc)
	a.Tick(context.Background())
	if r.peers[a.self].Acting {
		t.Fatal("setup: node0 isn't primary and must not act")
	}
	if len(inc.creates) != 0 || inc.memberLists != 0 {
		t.Errorf("non-primary: creates = %+v, member lists = %d; want neither", inc.creates, inc.memberLists)
	}
}

// A standalone host on an unclustered daemon: its only agent is the leader
// itself. There are no members to list, and a target would be rejected.
func TestUnclusteredDaemonCreatesNothing(t *testing.T) {
	inc := &fakeIncus{instances: []incuslocal.Instance{agentOn("agent-node0-g0", incuslocal.UnclusteredLocation, agentApp.Image)}}
	leaderTick(t, fleetSnapshot("node0", "node1"), inc)
	if len(inc.creates) != 0 || inc.memberLists != 0 {
		t.Errorf("creates = %+v, member lists = %d; want neither", inc.creates, inc.memberLists)
	}
}

// A standalone host as a one-member cluster: its own member already has an
// agent (this one), so the loop does nothing.
func TestOneMemberClusterCreatesNothing(t *testing.T) {
	inc := &fakeIncus{members: online("node0"), instances: []incuslocal.Instance{self()}}
	leaderTick(t, fleetSnapshot("node0"), inc)
	if len(inc.creates) != 0 {
		t.Errorf("creates = %+v, want none", inc.creates)
	}
}

func TestNoAgentAppDeclaredCreatesNothing(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{self()}}
	snap := fleetSnapshot("node0", "node1")
	snap.Config.Apps = nil
	leaderTick(t, snap, inc)
	if len(inc.creates) != 0 || inc.memberLists != 0 {
		t.Errorf("creates = %+v, member lists = %d; want neither", inc.creates, inc.memberLists)
	}
}

// logTo points a's log at a buffer and returns it.
func logTo(a *Agent) *bytes.Buffer {
	var buf bytes.Buffer
	a.log = log.New(&buf, "", 0)
	return &buf
}

func TestEachCreateHasADeadline(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1", "node2"), instances: []incuslocal.Instance{self()}}
	leaderTick(t, fleetSnapshot("node0", "node1", "node2"), inc)
	if len(inc.creates) != 2 {
		t.Fatalf("creates = %+v, want two", inc.creates)
	}
	for _, c := range inc.creates {
		if !c.hasDeadline {
			t.Errorf("create on %s ran without a deadline; a stalled pull would hold the tick", c.target)
		}
	}
}

// Peers groups agents by their own instance's tag. A leader tagged otherwise
// than the declared App would create agents that aren't its peers, with a
// second leader among them.
func TestLeaderTaggedOtherwiseThanTheAgentAppCreatesNothing(t *testing.T) {
	for name, own := range map[string]incuslocal.Instance{
		"App renamed in git":      self(), // tagged "agent"; the snapshot below declares "mgr"
		"own instance not listed": agentOn("agent-node9-g0", "node0", agentApp.Image),
	} {
		t.Run(name, func(t *testing.T) {
			inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{own}}
			snap := fleetSnapshot("node0", "node1")
			if name == "App renamed in git" {
				snap.Config.Apps[0].Name = "mgr"
			}
			leaderTick(t, snap, inc)
			if len(inc.creates) != 0 || inc.memberLists != 0 {
				t.Errorf("creates = %+v, member lists = %d; want neither", inc.creates, inc.memberLists)
			}
		})
	}
}

// A create that made the instance but failed to start it leaves a Stopped
// agent. It still counts (a create would collide on its name), but it is
// logged, once, rather than skipped silently.
func TestMemberWhoseAgentIsntRunningIsLoggedNotRecreated(t *testing.T) {
	stopped := agentOn("agent-node1-g0", "node1", agentApp.Image)
	stopped.Status = "Stopped"
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{self(), stopped}}
	snap := fleetSnapshot("node0", "node1")
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	buf := logTo(a)
	a.Tick(context.Background())
	a.Tick(context.Background())
	if len(inc.creates) != 0 {
		t.Errorf("creates = %+v, want none", inc.creates)
	}
	if got := strings.Count(buf.String(), "member node1 has an agent, but none is Running"); got != 1 {
		t.Errorf("not-Running note logged %d times over two ticks, want once:\n%s", got, buf)
	}
}

// A note for a member that has left the cluster is forgotten, so the same
// skip is logged again if it rejoins and recurs.
func TestNoteForADepartedMemberIsForgotten(t *testing.T) {
	inc := &fakeIncus{
		members:   []incuslocal.Member{{Name: "node0", Status: incuslocal.MemberOnline}, {Name: "node3", Status: "Offline"}},
		instances: []incuslocal.Instance{self()},
	}
	snap := fleetSnapshot("node0", "node3")
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	buf := logTo(a)
	a.Tick(context.Background())
	offline := inc.members[1]
	inc.members = inc.members[:1] // node3 removed from the cluster
	a.Tick(context.Background())
	inc.members = append(inc.members, offline) // rejoined, and Offline again
	a.Tick(context.Background())
	if got := strings.Count(buf.String(), "member node3 is Offline"); got != 2 {
		t.Errorf("node3's Offline skip logged %d times, want twice (before and after it left):\n%s", got, buf)
	}
}

// A create whose wait ends while Incus is still pulling is remembered, not
// sent again: a second create would block for another pass and then fail on
// the name. Once Incus lists the instance, Stopped while its rootfs unpacks,
// the member is reported as creating, not as an agent to start or delete.
func TestCreateStillRunningIsWaitedOnNotResent(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{self()}, pendingCreate: true}
	snap := fleetSnapshot("node0", "node1")
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	buf := logTo(a)

	a.Tick(context.Background()) // the pull outlasts the wait
	a.Tick(context.Background()) // still pulling: nothing listed yet
	unpacking := agentOn("agent-node1-g0", "node1", agentApp.Image)
	unpacking.Status = "Stopped"
	inc.instances = append(inc.instances, unpacking)
	a.Tick(context.Background())
	if len(inc.creates) != 1 {
		t.Fatalf("creates = %+v, want the one; a running create must not be sent again", inc.creates)
	}
	if strings.Contains(buf.String(), "none is Running") {
		t.Errorf("a member whose create is still running was reported as a stopped agent:\n%s", buf)
	}
	if !strings.Contains(buf.String(), "the create on member node1 (operation op-node1) is still running") {
		t.Errorf("no in-progress note for node1:\n%s", buf)
	}

	// Incus finishes and then forgets the operation (a 404).
	delete(inc.ops, "op-node1")
	inc.instances[len(inc.instances)-1].Status = "Running"
	a.Tick(context.Background())
	if len(inc.creates) != 1 || len(a.creating) != 0 {
		t.Errorf("creates = %d, creating = %v; want one create and nothing remembered", len(inc.creates), a.creating)
	}
}

// A remembered create that Incus reports failed is logged with Incus's
// reason and sent again in the same pass.
func TestCreateThatFailsAfterItsWaitIsLoggedAndRetried(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1"), instances: []incuslocal.Instance{self()}, pendingCreate: true}
	snap := fleetSnapshot("node0", "node1")
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	buf := logTo(a)
	a.Tick(context.Background())

	*inc.ops["op-node1"] = lxcapi.Operation{ID: "op-node1", Status: "Failure", StatusCode: lxcapi.Failure, Err: "image pull: 503"}
	inc.pendingCreate = false
	a.Tick(context.Background())
	if !strings.Contains(buf.String(), "ended Failure: image pull: 503") {
		t.Errorf("the failed create's reason wasn't logged:\n%s", buf)
	}
	if got := inc.targets(); !slices.Equal(got, []string{"node1", "node1"}) {
		t.Errorf("create targets = %q, want node1 sent again once its create failed", got)
	}
}

// Creates run one after another, so one deadline covers the pass: stalled
// pulls on three members hold the tick for one passTimeout, not three.
func TestOnePassDeadlineCoversEveryCreate(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node1", "node2", "node3"), instances: []incuslocal.Instance{self()}, stallCreate: true}
	snap := fleetSnapshot("node0", "node1", "node2", "node3")
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	a.passTimeout = 20 * time.Millisecond
	buf := logTo(a)
	a.Tick(context.Background())
	if got := inc.targets(); !slices.Equal(got, []string{"node1"}) {
		t.Errorf("create targets = %q, want only node1: its stall spent the pass", got)
	}
	if !strings.Contains(buf.String(), "the remaining members wait for next tick") {
		t.Errorf("the spent pass wasn't logged:\n%s", buf)
	}

	// node1's create is remembered, so the next pass moves on to node2.
	a.Tick(context.Background())
	if got := inc.targets(); !slices.Equal(got, []string{"node1", "node2"}) {
		t.Errorf("create targets after a second tick = %q, want node2 next", got)
	}
}

// At shutdown there is no next tick, so the members after the one whose
// create was interrupted aren't tried and logged as "retrying next tick".
func TestShutdownStopsTheMemberLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inc := &fakeIncus{members: online("node0", "node1", "node2", "node3"), instances: []incuslocal.Instance{self()}, onCreate: cancel}
	snap := fleetSnapshot("node0", "node1", "node2", "node3")
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}, inc)
	buf := logTo(a)
	a.ensureAgents(ctx, snap.Commit)
	if got := inc.targets(); !slices.Equal(got, []string{"node1"}) {
		t.Errorf("create targets = %q, want only node1, interrupted by the shutdown", got)
	}
	if strings.Contains(buf.String(), "next tick") {
		t.Errorf("a shutdown was logged as something for next tick:\n%s", buf)
	}
}

// A pass that creates nothing (here, no agent App declared) forgets the
// member notes too, so a skip that recurs once the App is back is logged
// again rather than taken as already said.
func TestSkipIsLoggedAgainAfterAPassThatCreatesNone(t *testing.T) {
	inc := &fakeIncus{members: online("node0", "node3"), instances: []incuslocal.Instance{self()}}
	snap := fleetSnapshot("node0")
	s := &fakeSync{snap: snap, have: map[string]bool{snap.Commit: true}}
	a, _ := testAgentWith(map[string]leaderelection.Peer{}, s, inc)
	buf := logTo(a)
	a.Tick(context.Background())
	s.snap.Config.Apps = nil
	a.Tick(context.Background())
	s.snap.Config.Apps = []config.App{agentApp}
	a.Tick(context.Background())
	if got := strings.Count(buf.String(), "member node3 is not a declared Instance"); got != 2 {
		t.Errorf("node3's skip logged %d times, want twice (before and after the App was gone):\n%s", got, buf)
	}
}
