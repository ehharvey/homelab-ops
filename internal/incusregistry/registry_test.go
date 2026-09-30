package incusregistry

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/leaderelection"
)

// fakeIncus is an in-memory instance table behind the Client interface. The
// wire itself is covered by incuslocal's own tests and by
// scripts/validate/incuslocal-round-trips-instances-over-unix-socket.sh.
type fakeIncus struct {
	instances []incuslocal.Instance
	writes    []map[string]string
	listErr   error
	setErr    error
}

func (f *fakeIncus) ListInstances(context.Context) ([]incuslocal.Instance, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.instances, nil
}

func (f *fakeIncus) SetUserKeys(_ context.Context, name string, keys map[string]string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.writes = append(f.writes, keys)
	for i := range f.instances {
		if f.instances[i].Name == name {
			for k, v := range keys {
				f.instances[i].Config[k] = v
			}
			return nil
		}
	}
	return errors.New("no such instance " + name)
}

func agentInst(name, status string, kv ...string) incuslocal.Instance {
	cfg := map[string]string{incuslocal.AppTagKey: "agent"}
	for i := 0; i+1 < len(kv); i += 2 {
		cfg[kv[i]] = kv[i+1]
	}
	return incuslocal.Instance{Name: name, Status: status, Location: "none", Config: cfg}
}

func TestRecordThenPeersRoundTrips(t *testing.T) {
	f := &fakeIncus{instances: []incuslocal.Instance{agentInst("agent-node0-g3", "Running")}}
	r := &Registry{Client: f, Self: "agent-node0-g3", Node: "node0", Generation: 3}
	ctx := context.Background()

	if err := r.Record(ctx, leaderelection.Publication{Commit: "abc", Acting: true}); err != nil {
		t.Fatal(err)
	}
	peers, err := r.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]leaderelection.Peer{"agent-node0-g3": {Node: "node0", Generation: 3, Commit: "abc", Acting: true}}
	if !reflect.DeepEqual(peers, want) {
		t.Fatalf("Peers = %+v, want %+v", peers, want)
	}

	// No ratchet: the commit may go backwards, and acting clears.
	if err := r.Record(ctx, leaderelection.Publication{Commit: "older"}); err != nil {
		t.Fatal(err)
	}
	peers, _ = r.Peers(ctx)
	if p := peers["agent-node0-g3"]; p.Commit != "older" || p.Acting {
		t.Errorf("after a second Record: %+v", p)
	}
}

// Only Running agent instances are peers. Anything else — Stopped, Error (an
// offline cluster member), Frozen, or an instance that never ran an agent —
// is left out, whatever it last published.
func TestPeersListsOnlyRunningAgents(t *testing.T) {
	published := []string{KeyNode, "node0", KeyGeneration, "0", KeyCommit, "c", KeyActing, "true"}
	f := &fakeIncus{instances: []incuslocal.Instance{
		agentInst("running", "Running", published...),
		agentInst("stopped", "Stopped", published...),
		agentInst("error", "Error", published...),
		agentInst("frozen", "Frozen", published...),
		agentInst("starting", "Starting", published...),
		agentInst("not-an-agent", "Running"), // App-tagged, never published
	}}
	peers, err := (&Registry{Client: f, Self: "running"}).Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || !peers["running"].Acting {
		t.Errorf("Peers = %+v, want only the running agent", peers)
	}
}

// Peers are scoped to this agent's own App: an agent of another App on the
// same Incus (a validate script's throwaway agents, another fleet's) is never
// a peer, whatever it publishes — even something this package can't parse.
func TestPeersAreOnlyThisAgentsApp(t *testing.T) {
	published := []string{KeyNode, "node0", KeyGeneration, "0", KeyCommit, "c", KeyActing, "true"}
	other := agentInst("other-fleet", "Running", published...)
	other.Config[incuslocal.AppTagKey] = "validate-agent-123"
	other.Config[KeyGeneration] = "not a number"
	f := &fakeIncus{instances: []incuslocal.Instance{
		agentInst("self", "Running", published...),
		agentInst("sibling", "Running", published...),
		other,
	}}
	peers, err := (&Registry{Client: f, Self: "self"}).Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 || !peers["self"].Acting || !peers["sibling"].Acting {
		t.Errorf("Peers = %+v, want self and sibling only", peers)
	}

	// Unlisted, an agent has no App to match, so no peers.
	peers, err = (&Registry{Client: f, Self: "missing"}).Peers(context.Background())
	if err != nil || len(peers) != 0 {
		t.Errorf("unlisted self: Peers = %+v, %v; want none", peers, err)
	}
}

func TestPeersRejectsMalformedState(t *testing.T) {
	for _, kv := range [][]string{
		{KeyNode, "node0"}, // no generation
		{KeyNode, "node0", KeyGeneration, "x"},
		{KeyNode, "node0", KeyGeneration, "0", KeyActing, "yes please"},
		{KeyNode, "node0", KeyGeneration, "0", KeyDraining, "2"},
	} {
		f := &fakeIncus{instances: []incuslocal.Instance{agentInst("a", "Running", kv...)}}
		if _, err := (&Registry{Client: f, Self: "a"}).Peers(context.Background()); err == nil {
			t.Errorf("Peers accepted %v", kv)
		}
	}
}

func TestDrainingAndHeartbeat(t *testing.T) {
	f := &fakeIncus{instances: []incuslocal.Instance{agentInst("a", "Running")}}
	r := &Registry{Client: f, Self: "a", Node: "node0"}
	ctx := context.Background()
	if err := r.Record(ctx, leaderelection.Publication{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDraining(ctx, true, DrainSyncFailure); err != nil {
		t.Fatal(err)
	}
	s, err := r.Own(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Found || !s.Peer.Draining || s.DrainReason != DrainSyncFailure || !s.VisibleToPeers() {
		t.Errorf("Self = %+v", s)
	}
	if err := r.SetDraining(ctx, false, "ignored"); err != nil {
		t.Fatal(err)
	}
	if s, _ := r.Own(ctx); s.Peer.Draining || s.DrainReason != "" {
		t.Errorf("after un-draining: %+v", s)
	}

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("x", 3600))
	if err := r.Heartbeat(ctx, now, "leader"); err != nil {
		t.Fatal(err)
	}
	cfg := f.instances[0].Config
	if cfg[KeyHeartbeat] != "2026-09-30T11:00:00Z" || cfg[KeyStatus] != "leader" {
		t.Errorf("heartbeat keys: %v", cfg)
	}
}

// Every write goes to the agent's own instance and stays inside user.*.
func TestWritesOnlyOwnUserKeys(t *testing.T) {
	f := &fakeIncus{instances: []incuslocal.Instance{agentInst("a", "Running"), agentInst("b", "Running")}}
	r := &Registry{Client: f, Self: "a", Node: "node0"}
	ctx := context.Background()
	_ = r.Record(ctx, leaderelection.Publication{Commit: "c"})
	_ = r.SetDraining(ctx, true, DrainSyncFailure)
	_ = r.Heartbeat(ctx, time.Now(), "s")
	if len(f.instances[1].Config) != 1 {
		t.Errorf("another instance was written: %v", f.instances[1].Config)
	}
	for _, w := range f.writes {
		for k := range w {
			if !strings.HasPrefix(k, "user.homelab-ops.agent.") {
				t.Errorf("wrote %q", k)
			}
		}
	}
}

func TestSelfReportsInvisibility(t *testing.T) {
	ctx := context.Background()
	f := &fakeIncus{instances: []incuslocal.Instance{agentInst("a", "Stopped")}}
	if s, err := (&Registry{Client: f, Self: "a"}).Own(ctx); err != nil || !s.Listed || s.VisibleToPeers() {
		t.Errorf("stopped own instance: %+v, %v", s, err)
	}
	if s, err := (&Registry{Client: f, Self: "untagged"}).Own(ctx); err != nil || s.Listed || s.VisibleToPeers() {
		t.Errorf("unlisted own instance: %+v, %v", s, err)
	}
}

func TestErrorsPropagate(t *testing.T) {
	boom := errors.New("boom")
	ctx := context.Background()
	r := &Registry{Client: &fakeIncus{listErr: boom, setErr: boom}, Self: "a"}
	if _, err := r.Peers(ctx); !errors.Is(err, boom) {
		t.Errorf("Peers err = %v", err)
	}
	if _, err := r.Own(ctx); !errors.Is(err, boom) {
		t.Errorf("Own err = %v", err)
	}
	if err := r.Record(ctx, leaderelection.Publication{}); !errors.Is(err, boom) {
		t.Errorf("Record err = %v", err)
	}
	if err := (&Registry{Client: &fakeIncus{}}).Record(ctx, leaderelection.Publication{}); err == nil {
		t.Error("Record with no Self must error")
	}
}
