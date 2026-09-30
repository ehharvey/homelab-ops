package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configsync"
	"github.com/ehharvey/homelab-ops/internal/incusregistry"
	"github.com/ehharvey/homelab-ops/internal/leaderelection"
)

// fakeSync stands in for a Clone: a queue of results for Sync, and a history.
type fakeSync struct {
	snap     configsync.Snapshot
	have     map[string]bool
	failing  bool // past the threshold: Sync and Current both error
	syncs    int
	onSync   func(*fakeSync) // runs on every Sync, e.g. to "fetch" a commit
	syncErr  error
	noSyncOK bool // Current errors (no successful sync yet)
}

func (f *fakeSync) Sync(context.Context) (configsync.Snapshot, error) {
	f.syncs++
	if f.onSync != nil {
		f.onSync(f)
	}
	if f.failing && f.syncErr == nil {
		return configsync.Snapshot{}, errors.New("git unreachable")
	}
	return f.snap, f.syncErr
}

func (f *fakeSync) Current() (configsync.Snapshot, error) {
	if f.noSyncOK || f.failing {
		return configsync.Snapshot{}, errors.New("sync failing")
	}
	return f.snap, nil
}

func (f *fakeSync) HasCommit(c string) (bool, error) { return f.have[c], nil }
func (f *fakeSync) Failing() bool                    { return f.failing }

// fakeReg is a shared in-memory table of peers.
type fakeReg struct {
	peers   map[string]leaderelection.Peer
	reasons map[string]string
	status  map[string]string
	self    string
	node    string
	gen     int64
	pubs    []leaderelection.Publication
}

func (r *fakeReg) Record(_ context.Context, p leaderelection.Publication) error {
	r.pubs = append(r.pubs, p)
	cur := r.peers[r.self]
	cur.Node, cur.Generation, cur.Commit, cur.Acting = r.node, r.gen, p.Commit, p.Acting
	r.peers[r.self] = cur
	return nil
}

func (r *fakeReg) Peers(context.Context) (map[string]leaderelection.Peer, error) {
	out := map[string]leaderelection.Peer{}
	for k, v := range r.peers {
		out[k] = v
	}
	return out, nil
}

func (r *fakeReg) Own(context.Context) (incusregistry.SelfState, error) {
	p, ok := r.peers[r.self]
	return incusregistry.SelfState{Listed: true, Status: "Running", Found: ok, Peer: p, DrainReason: r.reasons[r.self]}, nil
}

func (r *fakeReg) SetDraining(_ context.Context, d bool, reason string) error {
	p := r.peers[r.self]
	p.Draining = d
	r.peers[r.self] = p
	r.reasons[r.self] = reason
	return nil
}

func (r *fakeReg) Heartbeat(_ context.Context, _ time.Time, status string) error {
	r.status[r.self] = status
	return nil
}

func snapshot(primary, commit string) configsync.Snapshot {
	return configsync.Snapshot{Commit: commit, Config: config.Config{Designations: []config.Designation{{Primary: primary}}}}
}

func testAgent(peers map[string]leaderelection.Peer, s *fakeSync) (*Agent, *fakeReg) {
	r := &fakeReg{peers: peers, reasons: map[string]string{}, status: map[string]string{}, self: "agent-node0-g0", node: "node0"}
	if _, ok := peers[r.self]; !ok {
		peers[r.self] = leaderelection.Peer{Node: "node0"}
	}
	return New(r.self, r.node, r.gen, s, r, log.New(io.Discard, "", 0)), r
}

func TestStartClearsALeftoverActingFlagFirst(t *testing.T) {
	peers := map[string]leaderelection.Peer{"agent-node0-g0": {Node: "node0", Commit: "old", Acting: true}}
	a, r := testAgent(peers, &fakeSync{snap: snapshot("node1", "c1"), have: map[string]bool{"c1": true}})
	a.Tick(context.Background())
	if len(r.pubs) == 0 || r.pubs[0].Acting {
		t.Fatalf("first publication must clear acting: %+v", r.pubs)
	}
}

func TestTickLeadsAndHeartbeats(t *testing.T) {
	a, r := testAgent(map[string]leaderelection.Peer{}, &fakeSync{snap: snapshot("node0", "c1"), have: map[string]bool{"c1": true}})
	a.Tick(context.Background())
	if !r.peers[a.self].Acting || !a.leading {
		t.Fatal("the designated primary should be acting")
	}
	if !strings.HasPrefix(r.status[a.self], "leader:") {
		t.Errorf("status = %q", r.status[a.self])
	}
}

// A peer ahead makes the leader stop, clear acting, and re-sync within the
// same tick rather than waiting for the next poll.
func TestBehindStopsAndResyncsImmediately(t *testing.T) {
	s := &fakeSync{snap: snapshot("node0", "c1"), have: map[string]bool{"c1": true}}
	peers := map[string]leaderelection.Peer{}
	a, r := testAgent(peers, s)
	a.Tick(context.Background())
	if !r.peers[a.self].Acting {
		t.Fatal("setup: should lead")
	}

	peers["agent-node1-g0"] = leaderelection.Peer{Node: "node1", Commit: "c2"} // published the failover commit
	s.syncs = 0
	s.onSync = func(f *fakeSync) {
		if f.syncs == 2 { // the immediate re-sync fetches it
			f.snap, f.have["c2"] = snapshot("node1", "c2"), true
		}
	}
	a.Tick(context.Background())
	if s.syncs != 2 {
		t.Errorf("syncs this tick = %d, want 2 (poll + immediate re-sync)", s.syncs)
	}
	if r.peers[a.self].Acting || a.leading {
		t.Error("must have stopped acting")
	}
	if got := r.peers[a.self].Commit; got != "c2" {
		t.Errorf("published commit = %q, want c2 after the re-sync", got)
	}
}

// #187: persistent sync failure drains the agent; recovery un-drains it,
// unless a newer generation on its node took over meanwhile.
func TestDrainOnSyncFailure(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		s := &fakeSync{snap: snapshot("node0", "c1"), have: map[string]bool{"c1": true}}
		peers := map[string]leaderelection.Peer{}
		a, r := testAgent(peers, s)
		a.Tick(context.Background())

		s.failing = true
		a.Tick(context.Background())
		self := r.peers[a.self]
		if !self.Draining || self.Acting || r.reasons[a.self] != incusregistry.DrainSyncFailure {
			t.Fatalf("after threshold: %+v reason=%q", self, r.reasons[a.self])
		}

		if superseded {
			peers["agent-node0-g1"] = leaderelection.Peer{Node: "node0", Generation: 1, Commit: "c1"}
		}
		s.failing = false
		a.Tick(context.Background())
		self = r.peers[a.self]
		if superseded {
			if !self.Draining || r.reasons[a.self] != incusregistry.DrainSuperseded || self.Acting {
				t.Errorf("superseded: must stay drained: %+v reason=%q", self, r.reasons[a.self])
			}
			continue
		}
		if self.Draining || !self.Acting {
			t.Errorf("recovered: should un-drain and lead again: %+v", self)
		}
	}
}

// A restarted agent's sync failure count starts at zero, so "not Failing" is
// not recovery: an agent that restarts drained, with git still unreachable,
// stays drained until a sync actually succeeds.
func TestRestartWhileDrainedStaysDrainedUntilASyncSucceeds(t *testing.T) {
	s := &fakeSync{snap: snapshot("node0", "c1"), have: map[string]bool{}, noSyncOK: true, syncErr: errors.New("git unreachable")}
	peers := map[string]leaderelection.Peer{"agent-node0-g0": {Node: "node0", Draining: true}}
	a, r := testAgent(peers, s)
	r.reasons[a.self] = incusregistry.DrainSyncFailure

	a.Tick(context.Background())
	if !r.peers[a.self].Draining || r.reasons[a.self] != incusregistry.DrainSyncFailure {
		t.Fatalf("un-drained without a successful sync: %+v reason=%q", r.peers[a.self], r.reasons[a.self])
	}

	s.noSyncOK, s.syncErr, s.have["c1"] = false, nil, true
	a.Tick(context.Background())
	if r.peers[a.self].Draining || !r.peers[a.self].Acting {
		t.Errorf("a successful sync should un-drain it and let it lead: %+v", r.peers[a.self])
	}
}

// A drain for any reason but sync failure is never cleared by the loop.
func TestOtherDrainsAreNotCleared(t *testing.T) {
	peers := map[string]leaderelection.Peer{"agent-node0-g0": {Node: "node0", Draining: true}}
	a, r := testAgent(peers, &fakeSync{snap: snapshot("node0", "c1"), have: map[string]bool{"c1": true}})
	r.reasons[a.self] = "self-upgrade"
	a.Tick(context.Background())
	if !r.peers[a.self].Draining || r.peers[a.self].Acting {
		t.Errorf("%+v", r.peers[a.self])
	}
}

func TestShutdownClearsActing(t *testing.T) {
	a, r := testAgent(map[string]leaderelection.Peer{}, &fakeSync{snap: snapshot("node0", "c1"), have: map[string]bool{"c1": true}})
	a.Tick(context.Background())
	a.Shutdown(context.Background())
	if r.peers[a.self].Acting {
		t.Error("acting survived shutdown")
	}
}
