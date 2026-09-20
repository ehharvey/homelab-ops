package leaderelection

import (
	"context"
	"errors"
	"testing"
)

// fakeRegistry is an in-memory shared record. Each agent gets its own view
// (its name) over one map, mirroring one set of user.* keys per agent instance.
type fakeRegistry struct {
	shared    map[string]Peer
	self      string
	node      string
	gen       int64
	recordErr error
	readErr   error
}

func (f *fakeRegistry) Record(_ context.Context, epoch int64) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	p := f.shared[f.self]
	p.Node, p.Generation = f.node, f.gen
	if epoch > p.Epoch { // ratchet: never lowers
		p.Epoch = epoch
	}
	f.shared[f.self] = p
	return nil
}

func (f *fakeRegistry) Peers(_ context.Context) (map[string]Peer, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make(map[string]Peer, len(f.shared))
	for k, v := range f.shared {
		out[k] = v
	}
	return out, nil
}

func agent(self, node string, gen int64, shared map[string]Peer, des Designation) *Designated {
	return &Designated{
		Self: self, Node: node, Generation: gen,
		Source: func() (Designation, error) { return des, nil },
		Peers:  &fakeRegistry{shared: shared, self: self, node: node, gen: gen},
	}
}

func TestDesignatedMayAct(t *testing.T) {
	tests := []struct {
		name   string
		node   string
		des    Designation
		shared map[string]Peer
		want   bool
	}{
		{"primary at current epoch leads", "node0", Designation{"node0", 1}, map[string]Peer{}, true},
		{"non-primary does not lead", "node1", Designation{"node0", 1}, map[string]Peer{}, false},
		{"peer at the same epoch does not stop the primary", "node0", Designation{"node0", 2}, map[string]Peer{"x": {Node: "node1", Epoch: 2}}, true},
		{"peer at a lower epoch does not stop the primary", "node0", Designation{"node0", 2}, map[string]Peer{"x": {Node: "node1", Epoch: 1}}, true},
		{"stale designation: a peer already saw a higher epoch", "node0", Designation{"node0", 1}, map[string]Peer{"x": {Node: "node1", Epoch: 2}}, false},
		{"no primary designated", "node0", Designation{}, map[string]Peer{}, false},
		{"epoch zero is not a designation", "node0", Designation{"node0", 0}, map[string]Peer{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := agent("self", tt.node, 0, tt.shared, tt.des).MayAct(context.Background())
			if err != nil {
				t.Fatalf("MayAct: %v", err)
			}
			if got.Leader != tt.want {
				t.Errorf("Leader = %v, want %v (reason: %s)", got.Leader, tt.want, got.Reason)
			}
			if got.Reason == "" {
				t.Error("Reason is empty")
			}
		})
	}
}

// The failover the design exists for: an old primary on a stale git checkout
// stands down as soon as any peer has recorded the new epoch.
func TestDesignatedFailoverFencesStaleGit(t *testing.T) {
	shared := map[string]Peer{}
	oldStale := agent("agent-node0-g0", "node0", 0, shared, Designation{"node0", 1}) // has not synced the new designation
	newPrimary := agent("agent-node1-g0", "node1", 0, shared, Designation{"node1", 2})
	ctx := context.Background()

	if d, _ := oldStale.MayAct(ctx); !d.Leader {
		t.Fatalf("before failover the old primary should lead: %s", d.Reason)
	}
	// The operator has fenced node0's agent and raised the epoch; node1 syncs.
	if d, _ := newPrimary.MayAct(ctx); !d.Leader {
		t.Fatalf("new primary should lead once designated: %s", d.Reason)
	}
	// node0's agent comes back still on the stale checkout. It must stand down.
	if d, _ := oldStale.MayAct(ctx); d.Leader {
		t.Errorf("resurrected old primary must not lead at epoch 1 once epoch 2 is recorded: %s", d.Reason)
	}
}

// A blue-green self-upgrade puts the old (g0) and candidate (g1) instance on
// the primary node at once. Exactly one may act at every step, and the old one
// never has to retire itself.
func TestDesignatedSelfUpgradeHandoff(t *testing.T) {
	shared := map[string]Peer{}
	des := Designation{"node0", 1}
	old := agent("agent-node0-g0", "node0", 0, shared, des)
	cand := agent("agent-node0-g1", "node0", 1, shared, des)
	ctx := context.Background()
	leaders := func() (o, c bool) {
		do, _ := old.MayAct(ctx)
		dc, _ := cand.MayAct(ctx)
		return do.Leader, dc.Leader
	}

	if o, c := leaders(); !o || c {
		t.Fatalf("candidate just started: old should lead alone, got old=%v cand=%v", o, c)
	}

	// The old leader has seen the candidate sustained-healthy and steps aside.
	p := shared["agent-node0-g0"]
	p.Draining = true
	shared["agent-node0-g0"] = p
	if o, c := leaders(); o || !c {
		t.Fatalf("after draining: candidate should lead alone, got old=%v cand=%v", o, c)
	}

	// The candidate retires the old instance.
	delete(shared, "agent-node0-g0")
	if d, _ := cand.MayAct(ctx); !d.Leader {
		t.Errorf("candidate should keep leading once the old instance is gone: %s", d.Reason)
	}
}

func TestDesignatedRecordsEpochEvenWhenNotPrimary(t *testing.T) {
	shared := map[string]Peer{}
	if _, err := agent("agent-node1-g0", "node1", 0, shared, Designation{"node0", 3}).MayAct(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := shared["agent-node1-g0"].Epoch; got != 3 {
		t.Errorf("a follower must record the epoch it sees so a stale primary can learn of it; recorded %d, want 3", got)
	}
}

func TestDesignatedEpochRatchetNeverLowers(t *testing.T) {
	shared := map[string]Peer{"agent-node1-g0": {Node: "node1", Epoch: 5}}
	// node1 syncs an older designation (a git rollback): its record must stay at 5,
	// which in turn fences every agent still acting at a lower epoch.
	if _, err := agent("agent-node1-g0", "node1", 0, shared, Designation{"node1", 4}).MayAct(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := shared["agent-node1-g0"].Epoch; got != 5 {
		t.Errorf("epoch record lowered to %d, want it to stay 5", got)
	}
}

func TestDesignatedErrorsMeanNotLeader(t *testing.T) {
	boom := errors.New("boom")
	des := Designation{"node0", 1}
	src := func() (Designation, error) { return des, nil }
	reg := func() *fakeRegistry { return &fakeRegistry{shared: map[string]Peer{}, self: "s", node: "node0"} }
	withErr := func(f func(*fakeRegistry)) *fakeRegistry { r := reg(); f(r); return r }
	tests := []struct {
		name string
		d    *Designated
	}{
		{"source fails", &Designated{Self: "s", Node: "node0", Source: func() (Designation, error) { return Designation{}, boom }, Peers: reg()}},
		{"record fails", &Designated{Self: "s", Node: "node0", Source: src, Peers: withErr(func(r *fakeRegistry) { r.recordErr = boom })}},
		{"read fails", &Designated{Self: "s", Node: "node0", Source: src, Peers: withErr(func(r *fakeRegistry) { r.readErr = boom })}},
		{"empty self", &Designated{Node: "node0", Source: src, Peers: reg()}},
		{"empty node", &Designated{Self: "s", Source: src, Peers: reg()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.d.MayAct(context.Background())
			if err == nil {
				t.Error("want an error")
			}
			if got.Leader {
				t.Error("an unknown answer must never be Leader")
			}
		})
	}
}
