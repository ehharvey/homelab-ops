package leaderelection

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// world is the shared record every fake agent publishes to: one entry per
// agent instance, standing in for one set of user.* keys per Incus instance.
// Only running instances are in it; "stopping" an instance deletes its entry.
type world struct {
	mu    sync.Mutex
	peers map[string]Peer
}

func newWorld() *world { return &world{peers: map[string]Peer{}} }

func (w *world) set(name string, p Peer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.peers[name] = p
}

func (w *world) get(name string) Peer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peers[name]
}

func (w *world) snapshot() map[string]Peer {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]Peer, len(w.peers))
	for k, v := range w.peers {
		out[k] = v
	}
	return out
}

// actingCount is the property every scenario asserts on: how many running
// agents have Acting published at once.
func (w *world) actingCount() int {
	n := 0
	for _, p := range w.snapshot() {
		if p.Acting {
			n++
		}
	}
	return n
}

// fakeRegistry is one agent's view of a world.
type fakeRegistry struct {
	w    *world
	self string
	node string
	gen  int64

	recordErr error
	readErr   error
	// dropActing makes Acting writes invisible to reads (a stale read).
	dropActing bool
	// beforeClaim runs when Acting is about to be raised, before the write
	// lands; afterClaim runs once it has.
	beforeClaim, afterClaim func()
	// snapshotFirst, when set, answers every Peers read in place of the
	// world's current state, so a test can schedule or doctor reads.
	snapshotFirst func() map[string]Peer

	log []Publication
}

func (f *fakeRegistry) Record(_ context.Context, p Publication) error {
	f.log = append(f.log, p)
	if f.recordErr != nil {
		return f.recordErr
	}
	claiming := p.Acting && !f.w.get(f.self).Acting
	if claiming && f.beforeClaim != nil {
		f.beforeClaim()
	}
	f.w.mu.Lock()
	cur := f.w.peers[f.self]
	cur.Node, cur.Generation, cur.Commit = f.node, f.gen, p.Commit
	if !f.dropActing {
		cur.Acting = p.Acting
	}
	f.w.peers[f.self] = cur
	f.w.mu.Unlock()
	if claiming && f.afterClaim != nil {
		f.afterClaim()
	}
	return nil
}

func (f *fakeRegistry) Peers(_ context.Context) (map[string]Peer, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.snapshotFirst != nil {
		return f.snapshotFirst(), nil
	}
	return f.w.snapshot(), nil
}

// history is a fake git clone: the set of commits an agent has.
type history map[string]bool

func (h history) has(c string) (bool, error) { return h[c], nil }

// agent builds a Designated over w whose git history is hist and whose
// current designation is *des (a pointer, so a test can "sync" it forward).
func agent(w *world, self, node string, gen int64, des *Designation, hist history) (*Designated, *fakeRegistry) {
	reg := &fakeRegistry{w: w, self: self, node: node, gen: gen}
	// A running instance is in the world from the start, even before its
	// agent has published anything — which is what Incus shows.
	if _, ok := w.snapshot()[self]; !ok {
		w.set(self, Peer{Node: node, Generation: gen})
	}
	return &Designated{
		Self: self, Node: node, Generation: gen,
		Source:    func() (Designation, error) { return *des, nil },
		HasCommit: hist.has,
		Peers:     reg,
	}, reg
}

func mustMayAct(t *testing.T, d *Designated) Decision {
	t.Helper()
	dec, err := d.MayAct(context.Background())
	if err != nil {
		t.Fatalf("%s: MayAct: %v", d.Self, err)
	}
	if dec.Reason == "" {
		t.Errorf("%s: Reason is empty", d.Self)
	}
	return dec
}

// TestMayActRules is the four-part rule over peer states, from the point of
// view of one agent, "self", on node0 at generation 1 and commit c2, whose
// history holds c1 and c2.
func TestMayActRules(t *testing.T) {
	const self = "self"
	tests := []struct {
		name       string
		primary    string
		commit     string
		selfPeer   *Peer // nil: self is published normally; else overrides
		absentSelf bool  // self isn't in the running-peer list at all
		others     map[string]Peer
		want       bool
		wantBehind bool
		wantReason string
	}{
		{name: "primary alone leads", primary: "node0", want: true},
		{name: "non-primary does not lead", primary: "node1", wantReason: "designated primary is node1"},
		{name: "no primary designated", primary: "", wantReason: "no primary designated"},
		{name: "no commit to publish", primary: "node0", commit: "-", wantReason: "no commit"},

		// Rule 2: commit fencing.
		{name: "a peer ahead: stand down and re-sync", primary: "node0",
			others: map[string]Peer{"p": {Node: "node1", Commit: "c3"}}, wantBehind: true, wantReason: "behind: p is on commit c3"},
		{name: "a peer ahead stops a non-primary too, so it re-syncs", primary: "node1",
			others: map[string]Peer{"p": {Node: "node1", Commit: "c3"}}, wantBehind: true},
		{name: "a peer behind has no effect", primary: "node0",
			others: map[string]Peer{"p": {Node: "node1", Commit: "c1"}}, want: true},
		{name: "a peer on the same commit has no effect", primary: "node0",
			others: map[string]Peer{"p": {Node: "node1", Commit: "c2"}}, want: true},
		{name: "a peer that has published no commit yet has no effect", primary: "node0",
			others: map[string]Peer{"p": {Node: "node1"}}, want: true},

		// Rule 3: the acting handoff.
		{name: "another agent acting: wait", primary: "node0",
			others: map[string]Peer{"old": {Node: "node1", Commit: "c1", Acting: true}}, wantReason: "waiting for old to stop acting"},
		{name: "an acting agent on the same node, draining, still blocks", primary: "node0",
			others: map[string]Peer{"g0": {Node: "node0", Generation: 0, Commit: "c2", Acting: true, Draining: true}}, wantReason: "waiting for g0"},
		{name: "an acting peer that has stopped running is gone from the list", primary: "node0",
			others: map[string]Peer{}, want: true},

		// Rule 4: one instance per node.
		{name: "self draining", primary: "node0", selfPeer: &Peer{Node: "node0", Generation: 1, Commit: "c2", Draining: true}, wantReason: "draining"},
		{name: "older non-draining instance on this node leads", primary: "node0",
			others: map[string]Peer{"g0": {Node: "node0", Generation: 0, Commit: "c2"}}, wantReason: "older instance g0"},
		{name: "older draining instance on this node does not block", primary: "node0",
			others: map[string]Peer{"g0": {Node: "node0", Generation: 0, Commit: "c2", Draining: true}}, want: true},
		{name: "younger non-draining instance on this node does not block (asymmetric)", primary: "node0",
			others: map[string]Peer{"g2": {Node: "node0", Generation: 2, Commit: "c2"}}, want: true},
		{name: "older instance on another node does not block", primary: "node0",
			others: map[string]Peer{"x": {Node: "node1", Generation: 0, Commit: "c2"}}, want: true},

		// Leading needs to be seen.
		{name: "self not in the running-peer list", primary: "node0", absentSelf: true, wantReason: "not in the running-peer list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld()
			for name, p := range tt.others {
				w.set(name, p)
			}
			commit := "c2"
			switch tt.commit {
			case "-":
				commit = ""
			case "":
			default:
				commit = tt.commit
			}
			des := Designation{Primary: tt.primary, Commit: commit}
			d, reg := agent(w, self, "node0", 1, &des, history{"c1": true, "c2": true})
			if tt.selfPeer != nil {
				w.set(self, *tt.selfPeer)
			}
			if tt.absentSelf {
				reg.snapshotFirst = func() map[string]Peer {
					s := w.snapshot()
					delete(s, self)
					return s
				}
			}

			got := mustMayAct(t, d)
			if got.Leader != tt.want || got.Behind != tt.wantBehind {
				t.Errorf("Leader=%v Behind=%v, want Leader=%v Behind=%v (reason: %s)", got.Leader, got.Behind, tt.want, tt.wantBehind, got.Reason)
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
			if got.Commit != commit {
				t.Errorf("Decision.Commit = %q, want %q", got.Commit, commit)
			}
			// Leading means Acting is published; not leading means it isn't
			// (this agent never led, so there's nothing to stop).
			if pub := w.get(self).Acting; pub != tt.want || d.Acting() != tt.want {
				t.Errorf("published Acting=%v, Acting()=%v, want %v", pub, d.Acting(), tt.want)
			}
			// Every agent publishes its commit, leader or not.
			if tt.selfPeer == nil && !tt.absentSelf && w.get(self).Commit != commit {
				t.Errorf("published commit %q, want %q", w.get(self).Commit, commit)
			}
		})
	}
}

// The claim ordering: publish the commit, read; publish Acting, re-read; and
// only then answer Leader. A second call while acting re-checks without
// re-claiming.
func TestMayActClaimOrdering(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, reg := agent(w, "a", "node0", 0, &des, history{"c1": true})

	if dec := mustMayAct(t, d); !dec.Leader {
		t.Fatalf("want Leader: %s", dec.Reason)
	}
	want := []Publication{{Commit: "c1", Acting: false}, {Commit: "c1", Acting: true}}
	if !equalPubs(reg.log, want) {
		t.Fatalf("publications = %+v, want %+v", reg.log, want)
	}

	reg.log = nil
	if dec := mustMayAct(t, d); !dec.Leader {
		t.Fatalf("want Leader on re-check: %s", dec.Reason)
	}
	if want := []Publication{{Commit: "c1", Acting: true}}; !equalPubs(reg.log, want) {
		t.Errorf("publications while acting = %+v, want %+v", reg.log, want)
	}
}

// If something changes between the claim and the re-read, the claim is
// withdrawn at once: nothing has been done under it yet.
func TestMayActWithdrawsClaimWhenTheRecheckFails(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, reg := agent(w, "a", "node0", 1, &des, history{"c1": true})
	reg.afterClaim = func() { w.set("b", Peer{Node: "node1", Commit: "c1", Acting: true}) }

	dec := mustMayAct(t, d)
	if dec.Leader {
		t.Fatal("must not lead once another agent shows acting on the re-read")
	}
	if !strings.HasPrefix(dec.Reason, "withdrew claim: waiting for b") {
		t.Errorf("Reason = %q", dec.Reason)
	}
	if w.get("a").Acting || d.Acting() {
		t.Error("a withdrawn claim must leave Acting cleared")
	}
}

// A claim this agent can't see itself can't be seen by anyone else, so it
// fences nobody and is withdrawn.
func TestMayActWithdrawsWhenOwnFlagIsNotVisible(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, reg := agent(w, "a", "node0", 0, &des, history{"c1": true})
	reg.dropActing = true

	if dec := mustMayAct(t, d); dec.Leader || !strings.Contains(dec.Reason, "not yet visible") {
		t.Fatalf("want a withdrawn claim, got Leader=%v %q", dec.Leader, dec.Reason)
	}
	if d.Acting() {
		t.Error("Acting() after a withdrawn claim")
	}
}

// If the re-read after claiming fails, nothing has been done under the claim,
// so MayAct withdraws it itself rather than leaving the flag to block the
// real primary until the caller's Stop gets through.
func TestMayActWithdrawsClaimWhenTheReReadFails(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, reg := agent(w, "a", "node0", 0, &des, history{"c1": true})
	d.Peers = &failingReread{fakeRegistry: reg}

	if dec, err := d.MayAct(context.Background()); err == nil || dec.Leader {
		t.Fatalf("want an error and no Leader, got %+v, %v", dec, err)
	}
	if w.get("a").Acting || d.Acting() {
		t.Error("a claim whose re-read failed must be withdrawn")
	}
}

// failingReread fails every Peers read after the first.
type failingReread struct {
	*fakeRegistry
	reads int
}

func (f *failingReread) Peers(ctx context.Context) (map[string]Peer, error) {
	if f.reads++; f.reads > 1 {
		return nil, errors.New("re-read failed")
	}
	return f.fakeRegistry.Peers(ctx)
}

// An agent already acting gets the same own-flag check a fresh claim does:
// if a half-failed claim (and a failed Stop) left its flag unpublished, it
// must not act on a later tick until peers can see the flag.
func TestAlreadyActingNeedsOwnFlagVisible(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, reg := agent(w, "a", "node0", 0, &des, history{"c1": true})
	if dec := mustMayAct(t, d); !dec.Leader {
		t.Fatalf("setup: want Leader: %s", dec.Reason)
	}

	w.set("a", Peer{Node: "node0", Commit: "c1"}) // the flag isn't there
	reg.dropActing = true                         // and re-publishing it doesn't land
	dec := mustMayAct(t, d)
	if dec.Leader || !strings.Contains(dec.Reason, "not yet visible") {
		t.Fatalf("want not leader while its own flag is invisible, got Leader=%v %q", dec.Leader, dec.Reason)
	}
	if !d.Acting() {
		t.Error("work may be in flight, so the flag is the caller's to clear with Stop")
	}

	reg.dropActing = false
	if dec := mustMayAct(t, d); !dec.Leader {
		t.Errorf("once its flag is visible again it may act: %s", dec.Reason)
	}
}

// Once acting, a "not leader" answer leaves the flag published: the agent may
// still be mid-action, and only the caller knows when it has stopped.
func TestActingStaysPublishedUntilStop(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, _ := agent(w, "a", "node0", 0, &des, history{"c1": true})
	if dec := mustMayAct(t, d); !dec.Leader {
		t.Fatalf("want Leader: %s", dec.Reason)
	}

	w.set("b", Peer{Node: "node1", Commit: "c2"}) // b is ahead
	dec := mustMayAct(t, d)
	if dec.Leader || !dec.Behind {
		t.Fatalf("want not leader and Behind, got %+v", dec)
	}
	if !w.get("a").Acting || !d.Acting() {
		t.Fatal("Acting must stay published until the caller has stopped")
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := w.get("a"); p.Acting || p.Commit != "c1" || d.Acting() {
		t.Errorf("after Stop: published %+v, Acting()=%v; want acting cleared and commit kept", p, d.Acting())
	}
}

// On startup an agent clears a flag its previous run left behind, before it
// has synced anything.
func TestStopAtStartupClearsALeftoverFlag(t *testing.T) {
	w := newWorld()
	w.set("a", Peer{Node: "node0", Commit: "c0", Acting: true}) // a previous run's
	des := Designation{}
	d, _ := agent(w, "a", "node0", 0, &des, history{})
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := w.get("a"); p.Acting {
		t.Errorf("leftover acting flag survived Stop: %+v", p)
	}
}

// Two claimants at exactly the same moment: both read a clean slate, both
// publish Acting, both re-read. Each sees the other and both back off — safe,
// and the next tick settles it once one of them is out of the running.
func TestSimultaneousClaimantsBothBackOff(t *testing.T) {
	w := newWorld()
	// Both believe they're primary on unrelated histories that each claim to
	// hold the other's commit: rules 1, 2 and 4 all pass for both, leaving
	// rule 3 as the only thing between them.
	desA := Designation{Primary: "node0", Commit: "ca"}
	desB := Designation{Primary: "node1", Commit: "cb"}
	both := history{"ca": true, "cb": true}
	a, ra := agent(w, "a", "node0", 0, &desA, both)
	b, rb := agent(w, "b", "node1", 0, &desB, both)

	// Without the third barrier this is the other legal interleaving: one
	// withdraws before the other re-reads, and the other leads (see
	// TestRacingClaimantsNeverBothLead).
	firstRead, claimed, reRead := newBarrier(2), newBarrier(2), newBarrier(2)
	for _, r := range []*fakeRegistry{ra, rb} {
		reads := 0
		r.snapshotFirst = func() map[string]Peer {
			s := w.snapshot()
			reads++
			switch reads {
			case 1:
				firstRead.wait() // both take their first read before either claims
			case 2:
				reRead.wait() // both re-read before either withdraws
			}
			return s
		}
		r.afterClaim = claimed.wait // both claims land before either re-reads
	}

	decs := runConcurrently(a, b)
	for i, dec := range decs {
		if dec.Leader {
			t.Errorf("agent %d led: %s", i, dec.Reason)
		}
		if !strings.Contains(dec.Reason, "withdrew claim") {
			t.Errorf("agent %d: Reason = %q, want a withdrawn claim", i, dec.Reason)
		}
	}
	if n := w.actingCount(); n != 0 {
		t.Errorf("%d agents left Acting published after backing off", n)
	}
}

// Two claimants racing without any imposed schedule: over many rounds, never
// two leaders at once.
func TestRacingClaimantsNeverBothLead(t *testing.T) {
	for round := 0; round < 200; round++ {
		w := newWorld()
		desA := Designation{Primary: "node0", Commit: "ca"}
		desB := Designation{Primary: "node1", Commit: "cb"}
		both := history{"ca": true, "cb": true}
		a, _ := agent(w, "a", "node0", 0, &desA, both)
		b, _ := agent(w, "b", "node1", 0, &desB, both)
		decs := runConcurrently(a, b)
		if decs[0].Leader && decs[1].Leader {
			t.Fatalf("round %d: both led", round)
		}
	}
}

// One claimant slips its whole claim in between the other's first read and
// its claim landing: the late one's re-read catches it. At most one leads.
func TestInterleavedClaimantsAtMostOneLeads(t *testing.T) {
	w := newWorld()
	desA := Designation{Primary: "node0", Commit: "ca"}
	desB := Designation{Primary: "node1", Commit: "cb"}
	both := history{"ca": true, "cb": true}
	a, ra := agent(w, "a", "node0", 0, &desA, both)
	b, _ := agent(w, "b", "node1", 0, &desB, both)
	var bDec Decision
	ra.beforeClaim = func() { bDec = mustMayAct(t, b) } // b runs to completion before a's claim lands

	aDec := mustMayAct(t, a)
	if !bDec.Leader {
		t.Fatalf("b saw a clean slate and should lead: %s", bDec.Reason)
	}
	if aDec.Leader {
		t.Fatalf("a's re-read must see b acting: %s", aDec.Reason)
	}
}

// A force-push: agents on the old history and the new each see the other's
// commit as unknown. Both stand down (zero leaders, never two) until they
// converge.
func TestForcePushStandsBothSidesDown(t *testing.T) {
	w := newWorld()
	old := Designation{Primary: "node0", Commit: "old2"}
	rewritten := Designation{Primary: "node0", Commit: "new2"}
	n0, _ := agent(w, "n0", "node0", 0, &old, history{"c1": true, "old2": true})
	n1, _ := agent(w, "n1", "node1", 0, &rewritten, history{"c1": true, "new2": true})

	mustMayAct(t, n0) // publishes old2; nothing ahead of it yet
	if d := mustMayAct(t, n1); d.Leader || !d.Behind {
		t.Fatalf("n1 must see old2 as unknown: %+v", d)
	}
	d0 := mustMayAct(t, n0)
	if d0.Leader || !d0.Behind {
		t.Fatalf("n0 must see new2 as unknown and stand down: %+v", d0)
	}
	if err := n0.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := w.actingCount(); n != 0 {
		t.Fatalf("%d acting during a force-push stall", n)
	}

	// n0 re-syncs onto the rewritten history; the fleet converges.
	old = Designation{Primary: "node0", Commit: "new2"}
	n0.HasCommit = history{"c1": true, "old2": true, "new2": true}.has
	if d := mustMayAct(t, n0); !d.Leader {
		t.Fatalf("after converging, node0 should lead: %s", d.Reason)
	}
}

// A draining peer's commit doesn't fence. One drained for sync failure can't
// republish, so after a force-push it would otherwise hold everyone Behind on
// a commit nobody has until it reached git again.
func TestDrainingPeerOnAnAbandonedCommitDoesNotFence(t *testing.T) {
	w := newWorld()
	w.set("stuck", Peer{Node: "node1", Commit: "old2", Draining: true})
	des := Designation{Primary: "node0", Commit: "new2"}
	d, _ := agent(w, "a", "node0", 0, &des, history{"c1": true, "new2": true})

	if dec := mustMayAct(t, d); !dec.Leader || dec.Behind {
		t.Fatalf("a draining peer's abandoned commit must not stand it down: %+v", dec)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Still acting, it still fences — through rule 3.
	w.set("stuck", Peer{Node: "node1", Commit: "old2", Draining: true, Acting: true})
	if dec := mustMayAct(t, d); dec.Leader || dec.Behind || !strings.Contains(dec.Reason, "waiting for stuck") {
		t.Errorf("a draining peer still acting must block: %+v", dec)
	}
}

// The failover the design exists for: one commit changes primary from node0
// to node1. At no step do two agents have Acting published.
func TestFailoverHandoffNeverOverlaps(t *testing.T) {
	w := newWorld()
	des0 := Designation{Primary: "node0", Commit: "c1"}
	des1 := Designation{Primary: "node0", Commit: "c1"}
	h0, h1 := history{"c1": true}, history{"c1": true}
	n0, _ := agent(w, "agent-node0-g0", "node0", 0, &des0, h0)
	n1, _ := agent(w, "agent-node1-g0", "node1", 0, &des1, h1)
	step := func(d *Designated) Decision {
		dec := mustMayAct(t, d)
		// The caller's half: not leader while acting means stop, then Stop.
		if !dec.Leader && d.Acting() {
			if err := d.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if n := w.actingCount(); n > 1 {
			t.Fatalf("%d agents acting at once", n)
		}
		return dec
	}

	if !step(n0).Leader || step(n1).Leader {
		t.Fatal("before failover node0 alone leads")
	}

	// The operator commits primary: node1. node1 syncs first.
	h1["c2"] = true
	des1 = Designation{Primary: "node1", Commit: "c2"}
	if d := step(n1); d.Leader || !strings.Contains(d.Reason, "waiting for agent-node0-g0") {
		t.Fatalf("node1 must wait for node0 to stop acting: %+v", d)
	}
	// node0's next tick: node1 is ahead, so node0 stops and re-syncs.
	if d := step(n0); d.Leader || !d.Behind {
		t.Fatalf("node0 must stand down as behind: %+v", d)
	}
	h0["c2"] = true
	des0 = Designation{Primary: "node1", Commit: "c2"}
	if d := step(n0); d.Leader {
		t.Fatalf("node0, re-synced, is no longer primary: %+v", d)
	}
	if d := step(n1); !d.Leader {
		t.Fatalf("node1 takes over once node0 has stopped: %s", d.Reason)
	}
	if d := step(n0); d.Leader {
		t.Fatal("node0 stays out")
	}
}

// A resurrected old primary on a stale checkout: nobody is acting, it still
// believes it's primary, and only the fence — a peer on a commit it lacks —
// stands it down.
func TestStaleCheckoutStandsDown(t *testing.T) {
	w := newWorld()
	w.set("agent-node1-g0", Peer{Node: "node1", Commit: "c2"}) // on the new commit, not acting
	stale := Designation{Primary: "node0", Commit: "c1"}
	n0, _ := agent(w, "agent-node0-g0", "node0", 0, &stale, history{"c1": true})
	d := mustMayAct(t, n0)
	if d.Leader || !d.Behind {
		t.Fatalf("a stale-checkout primary must stand down: %+v", d)
	}
}

// A blue-green self-upgrade puts the old (g0) and candidate (g1) instance on
// the primary node at once. Exactly one may act at every step, the candidate
// waits for the old one's Acting to clear, and the old one never retires
// itself.
func TestSelfUpgradeHandoff(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	hist := history{"c1": true}
	old, _ := agent(w, "agent-node0-g0", "node0", 0, &des, hist)
	if d := mustMayAct(t, old); !d.Leader {
		t.Fatalf("old should lead: %s", d.Reason)
	}
	cand, _ := agent(w, "agent-node0-g1", "node0", 1, &des, hist)
	if d := mustMayAct(t, cand); d.Leader {
		t.Fatal("candidate just started: old leads alone")
	}

	// The old leader sees its candidate sustained-healthy and drains.
	p := w.get("agent-node0-g0")
	p.Draining = true
	w.set("agent-node0-g0", p)
	if d := mustMayAct(t, old); d.Leader {
		t.Fatal("a draining instance must not lead")
	}
	// It hasn't published Stop yet: the candidate still waits.
	if d := mustMayAct(t, cand); d.Leader || !strings.Contains(d.Reason, "waiting for agent-node0-g0") {
		t.Fatalf("candidate must wait for the old one's acting to clear: %+v", d)
	}
	if err := old.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := mustMayAct(t, cand); !d.Leader {
		t.Fatalf("candidate leads once the old one has stopped: %s", d.Reason)
	}
	if n := w.actingCount(); n != 1 {
		t.Fatalf("%d acting", n)
	}
}

func TestMayActErrorsMeanNotLeader(t *testing.T) {
	boom := errors.New("boom")
	des := Designation{Primary: "node0", Commit: "c1"}
	src := func() (Designation, error) { return des, nil }
	has := history{"c1": true}.has
	reg := func(f func(*fakeRegistry)) *fakeRegistry {
		w := newWorld()
		w.set("s", Peer{Node: "node0"})
		w.set("p", Peer{Node: "node1", Commit: "cX"})
		r := &fakeRegistry{w: w, self: "s", node: "node0"}
		if f != nil {
			f(r)
		}
		return r
	}
	tests := []struct {
		name string
		d    *Designated
	}{
		{"source fails", &Designated{Self: "s", Node: "node0", Source: func() (Designation, error) { return Designation{}, boom }, HasCommit: has, Peers: reg(nil)}},
		{"record fails", &Designated{Self: "s", Node: "node0", Source: src, HasCommit: has, Peers: reg(func(r *fakeRegistry) { r.recordErr = boom })}},
		{"read fails", &Designated{Self: "s", Node: "node0", Source: src, HasCommit: has, Peers: reg(func(r *fakeRegistry) { r.readErr = boom })}},
		{"commit lookup fails", &Designated{Self: "s", Node: "node0", Source: src, HasCommit: func(string) (bool, error) { return false, boom }, Peers: reg(nil)}},
		{"empty self", &Designated{Node: "node0", Source: src, HasCommit: has, Peers: reg(nil)}},
		{"empty node", &Designated{Self: "s", Source: src, HasCommit: has, Peers: reg(nil)}},
		{"no HasCommit", &Designated{Self: "s", Node: "node0", Source: src, Peers: reg(nil)}},
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

// A claim whose write failed may still have landed, so the agent treats
// itself as acting and owes a Stop.
func TestFailedClaimWriteStillOwesAStop(t *testing.T) {
	w := newWorld()
	des := Designation{Primary: "node0", Commit: "c1"}
	d, reg := agent(w, "a", "node0", 0, &des, history{"c1": true})
	d.Peers = recordHook{reg, func(p Publication) error {
		if p.Acting {
			return errors.New("timeout after write")
		}
		return nil
	}}
	if _, err := d.MayAct(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if !d.Acting() {
		t.Error("a failed Acting write must be assumed landed")
	}
	if err := d.Stop(context.Background()); err != nil || d.Acting() {
		t.Errorf("Stop: err=%v Acting()=%v", err, d.Acting())
	}
}

type recordHook struct {
	*fakeRegistry
	fail func(Publication) error
}

func (r recordHook) Record(ctx context.Context, p Publication) error {
	if err := r.fail(p); err != nil {
		return err
	}
	return r.fakeRegistry.Record(ctx, p)
}

func TestMayUndrain(t *testing.T) {
	tests := []struct {
		name  string
		peers map[string]Peer
		want  bool
	}{
		{"alone", map[string]Peer{"self": {Node: "node0", Generation: 1, Draining: true}}, true},
		{"a newer non-draining generation took over", map[string]Peer{"g2": {Node: "node0", Generation: 2}}, false},
		{"a newer generation that is itself draining", map[string]Peer{"g2": {Node: "node0", Generation: 2, Draining: true}}, true},
		{"an older non-draining generation", map[string]Peer{"g0": {Node: "node0", Generation: 0}}, true},
		{"a newer generation on another node", map[string]Peer{"x": {Node: "node1", Generation: 5}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := MayUndrain("self", "node0", 1, tt.peers)
			if got != tt.want {
				t.Errorf("MayUndrain = %v (%s), want %v", got, reason, tt.want)
			}
		})
	}
}

// --- helpers ---

type barrier struct {
	mu sync.Mutex
	n  int
	ch chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, ch: make(chan struct{})} }

func (b *barrier) wait() {
	b.mu.Lock()
	b.n--
	if b.n == 0 {
		close(b.ch)
	}
	b.mu.Unlock()
	<-b.ch
}

func runConcurrently(ds ...*Designated) []Decision {
	out := make([]Decision, len(ds))
	var wg sync.WaitGroup
	for i, d := range ds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = d.MayAct(context.Background())
		}()
	}
	wg.Wait()
	return out
}

func equalPubs(a, b []Publication) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
