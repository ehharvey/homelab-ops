package agent

import (
	"slices"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/clusterruntime"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
)

// recordingRuntime runs the Incus runtime and records which of its
// operations the leader work asked it for.
type recordingRuntime struct {
	clusterruntime.Runtime
	calls []string
}

func (r *recordingRuntime) Join(p clusterruntime.LeaderPass) {
	r.calls = append(r.calls, "join")
	r.Runtime.Join(p)
}

func (r *recordingRuntime) Upgrade(p clusterruntime.LeaderPass) (bool, []string) {
	r.calls = append(r.calls, "upgrade")
	return r.Runtime.Upgrade(p)
}

func (r *recordingRuntime) Health(c clusterruntime.Candidate) bool {
	r.calls = append(r.calls, "health")
	return r.Runtime.Health(c)
}

// TestLeaderWorkRunsTheSetRuntime: a set Agent.Runtime is what a leader tick
// runs, Join before Upgrade and Health from within Upgrade, with the same
// effect as a nil one (TestHealthySinceIsSetOnAFreshHeartbeatAndClearedOnAStaleOne).
func TestLeaderWorkRunsTheSetRuntime(t *testing.T) {
	inc := &fakeIncus{members: online("node0"), location: "node0", instances: []incuslocal.Instance{
		agentGen("agent-node0-g0", "node0", 0, staleImage),
		with(candidate("agent-node0-g1", "node0"), incusregistry.KeyHeartbeat, rfc(t0)),
	}}
	a, r, _ := upgradeLeader("agent-node0-g0", 0, fleetSnapshot("node0"), inc, nil)
	rt := &recordingRuntime{Runtime: clusterruntime.Incus}
	a.Runtime = rt

	tick(t, a, r)
	if want := []string{"join", "upgrade", "health"}; !slices.Equal(rt.calls, want) {
		t.Errorf("runtime calls = %q, want %q", rt.calls, want)
	}
	since := upgradeKey("node0", upgradeHealthySince)
	if got := inc.instances[inc.find(a.self)].Config[since]; got != rfc(t0) {
		t.Errorf("%s = %q after a fresh heartbeat, want %q", since, got, rfc(t0))
	}
}
