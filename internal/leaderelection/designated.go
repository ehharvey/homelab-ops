package leaderelection

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Designation is the operator's statement of who leads — a primary node —
// together with the git commit it was read at. It comes from the agent's git
// sync; where it's parsed from is the caller's concern (`kind: Designation`,
// docs/Config Schema.md), not this package's.
//
// There is no epoch (#212). A stale checkout is fenced by Commit instead: each
// agent publishes the commit its designation came from, and an agent that
// sees a peer on a commit outside its own history stands down. Failing over is
// one commit changing Primary.
type Designation struct {
	// Primary is the designated node, or "" for none (no agent leads).
	Primary string
	// Commit is the git commit Primary was read at: the agent's last
	// successful sync's HEAD.
	Commit string
}

// Peer is what one agent instance publishes about itself. Each field is
// written by that instance alone (a user.* key on its own Incus instance in
// the Incus-backed implementation), so no compare-and-swap is needed.
type Peer struct {
	// Node is the node the instance runs on, matched against
	// Designation.Primary.
	Node string
	// Generation is the instance's blue-green generation on that node. During a
	// self-upgrade the old and the candidate instance both run there; the
	// oldest live, non-draining one leads.
	Generation int64
	// Commit is the git commit the instance's designation came from, or "" if
	// it hasn't synced since it started. It may move backwards (a force-push
	// rollback): there is no ratchet.
	Commit string
	// Acting means the instance may be performing leader-only work. It's set
	// before the first action and cleared only after the instance has fully
	// stopped, so a new primary that waits for it to clear never overlaps the
	// old one.
	Acting bool
	// Draining means the instance has stopped acting for good or until it
	// recovers: a leader handing over to its sustained-healthy candidate
	// (#109), or an agent whose git sync keeps failing (#187).
	Draining bool
}

// Publication is what an agent writes about itself on each Record.
type Publication struct {
	// Commit is the commit its current designation came from.
	Commit string
	// Acting is whether it may be performing leader-only work.
	Acting bool
}

// Registry is the shared record agents publish to and read from.
type Registry interface {
	// Record publishes this agent's commit and acting state on its own
	// record, unconditionally. There is no ratchet: the commit may move
	// backwards, and Record just writes it. The implementation fills in Node
	// and Generation, which it knows.
	Record(ctx context.Context, p Publication) error
	// Peers returns the published state of every running agent, keyed by
	// instance name, including this agent's own. An instance that is not
	// running must not appear: a dead old primary's leftover Acting flag
	// would otherwise block every successor, and a dead older generation
	// would block its candidate. "Running" must mean positively reported as
	// running, not merely "not stopped" — docs/Decisions.md §25's #212
	// addendum depends on an offline cluster member's instances dropping out.
	Peers(ctx context.Context) (map[string]Peer, error)
}

// Designated is the Elector for operator-designated leadership.
//
// An agent may act iff, all at once (docs/Decisions.md §25, #212 addendum):
//  1. its designation, from its own last successful sync, names its node as
//     primary;
//  2. every running, non-draining peer's published commit is in its own
//     history (a peer on a commit it lacks is ahead of it: it stands down
//     and reports Decision.Behind so the caller re-syncs now);
//  3. no other running peer shows Acting; and
//  4. it isn't draining, and no older non-draining instance runs on its node.
//
// MayAct publishes Acting before it first answers Leader, then re-reads its
// peers and re-checks all four: of two agents claiming at once, at least one
// sees the other (the Dekker ordering). When a later answer is "not leader",
// the Acting flag stays published until the caller calls Stop, which it must
// do only once its in-flight work has finished or been abandoned.
//
// This is a deterministic function of its inputs and holds no timers. There is
// deliberately no timeout on Acting: a hung primary blocks takeover until an
// operator stops it, rather than risking a frozen process waking mid-action
// after its successor started.
//
// Not safe for concurrent use: one agent loop owns one Designated.
type Designated struct {
	// Self is this agent's instance name, its key in Registry.Peers.
	Self string
	// Node and Generation identify where and which generation this agent is.
	Node       string
	Generation int64
	// Source returns the current designation from the agent's git sync. It
	// should error once sync has failed persistently (#187), which makes this
	// agent "not leader" without needing git to know it.
	Source func() (Designation, error)
	// HasCommit reports whether commit is in this agent's own git history —
	// the lookup behind rule 2.
	HasCommit func(commit string) (bool, error)
	// Peers is the shared record.
	Peers Registry

	// acting is what this agent last published (or may have published: a
	// failed write is assumed to have landed) for its Acting flag.
	acting bool
	// commit is the commit this agent last published.
	commit string
}

var _ Elector = (*Designated)(nil)

// Acting reports whether this agent's Acting flag is (or may be) published as
// true — whether the caller owes a Stop once it has stopped its work.
func (d *Designated) Acting() bool { return d.acting }

// MayAct implements Elector, claiming Acting on the way to a Leader answer.
//
// A Leader answer means Acting is published and every rule held on a read
// taken after publishing it. A non-Leader answer (or an error) while Acting is
// published leaves it published: the caller must stop its in-flight work and
// then call Stop.
func (d *Designated) MayAct(ctx context.Context) (Decision, error) {
	if d.Self == "" || d.Node == "" {
		return Decision{}, errors.New("leaderelection: Designated.Self and Node must be set")
	}
	if d.Source == nil || d.HasCommit == nil || d.Peers == nil {
		return Decision{}, errors.New("leaderelection: Designated.Source, HasCommit and Peers must be set")
	}
	des, err := d.Source()
	if err != nil {
		return Decision{}, fmt.Errorf("leaderelection: reading designation: %w", err)
	}

	// Publish before reading: an agent's commit is the evidence that fences
	// a peer behind it, so every agent publishes it, primary or not.
	if err := d.record(ctx, Publication{Commit: des.Commit, Acting: d.acting}); err != nil {
		return Decision{}, err
	}
	peers, err := d.Peers.Peers(ctx)
	if err != nil {
		return Decision{}, fmt.Errorf("leaderelection: reading peers: %w", err)
	}
	dec, err := d.evaluate(des, peers)
	if err != nil || !dec.Leader {
		return dec, err
	}
	if d.acting {
		// Already claimed, so the Record above re-published Acting before
		// this read: it's a read after publishing, as a fresh claim's re-read
		// is, and needs the same visibility check. A non-Leader answer here
		// isn't withdrawn, since work may be in flight; the caller's Stop is.
		return d.ownFlagVisible(dec, des, peers), nil
	}

	// Claim: publish Acting, then re-read and re-check everything before the
	// first action.
	if err := d.record(ctx, Publication{Commit: des.Commit, Acting: true}); err != nil {
		return Decision{}, err
	}
	if peers, err = d.Peers.Peers(ctx); err != nil {
		err = fmt.Errorf("leaderelection: re-reading peers after claiming: %w", err)
	} else if dec, err = d.evaluate(des, peers); err == nil {
		dec = d.ownFlagVisible(dec, des, peers)
	}
	if err != nil || !dec.Leader {
		// Nothing has been done under this claim, so it can be withdrawn at
		// once. If the withdrawal fails, Acting() stays true and the caller's
		// Stop retries it.
		if werr := d.record(ctx, Publication{Commit: des.Commit, Acting: false}); werr != nil {
			return Decision{}, errors.Join(err, werr)
		}
		if err != nil {
			return Decision{}, err
		}
		dec.Reason = "withdrew claim: " + dec.Reason
		return dec, nil
	}
	return dec, nil
}

// ownFlagVisible demotes a Leader answer whose read doesn't show this agent's
// own Acting flag: then no peer can see it either, so it fences nobody.
func (d *Designated) ownFlagVisible(dec Decision, des Designation, peers map[string]Peer) Decision {
	if dec.Leader && !peers[d.Self].Acting {
		return Decision{Commit: des.Commit, Reason: "own acting flag not yet visible after publishing it"}
	}
	return dec
}

// Stop publishes Acting false. Call it once in-flight leader work has
// finished or been abandoned after a non-Leader answer — and at startup,
// before anything else, since a flag left by a previous run is this agent's
// own and it isn't acting.
func (d *Designated) Stop(ctx context.Context) error {
	if d.Peers == nil {
		return errors.New("leaderelection: Designated.Peers must be set")
	}
	return d.record(ctx, Publication{Commit: d.commit, Acting: false})
}

// record publishes p and tracks what's published. A failed write that meant
// to raise Acting is assumed to have landed, so the caller still owes a Stop.
func (d *Designated) record(ctx context.Context, p Publication) error {
	if p.Acting {
		d.acting = true
	}
	if err := d.Peers.Record(ctx, p); err != nil {
		return fmt.Errorf("leaderelection: publishing commit %q acting=%v: %w", p.Commit, p.Acting, err)
	}
	d.acting, d.commit = p.Acting, p.Commit
	return nil
}

// evaluate applies the four-part rule to one read of the peers.
func (d *Designated) evaluate(des Designation, peers map[string]Peer) (Decision, error) {
	names := slices.Sorted(maps.Keys(peers)) // deterministic reasons

	undecided := func(reason string, args ...any) Decision {
		return Decision{Commit: des.Commit, Reason: fmt.Sprintf(reason, args...)}
	}

	// Rule 2 first, for every agent, primary or not: Behind is what tells a
	// follower to re-sync now too, and a stale follower is exactly the
	// designation evidence a stale primary would otherwise lack.
	//
	// A draining peer's commit isn't evidence. One drained for sync failure
	// can't republish (its Source errors before Record), so after a
	// force-push its old commit would sit outside everyone's history and
	// hold every agent Behind until it reached git again. A draining peer
	// that is still acting fences through rule 3 regardless.
	for _, name := range names {
		p := peers[name]
		if name == d.Self || p.Draining || p.Commit == "" || p.Commit == des.Commit {
			continue
		}
		has, err := d.HasCommit(p.Commit)
		if err != nil {
			return Decision{}, fmt.Errorf("leaderelection: looking up %s's commit %s: %w", name, p.Commit, err)
		}
		if !has {
			dec := undecided("behind: %s is on commit %s, which isn't in my history", name, short(p.Commit))
			dec.Behind = true
			return dec, nil
		}
	}

	self, visible := peers[d.Self]
	switch {
	case des.Commit == "":
		return undecided("designation has no commit to publish"), nil
	case des.Primary == "":
		return undecided("no primary designated"), nil
	case des.Primary != d.Node:
		return undecided("designated primary is %s", des.Primary), nil
	case !visible:
		// Peers couldn't see this agent acting, so its flag would fence
		// nobody: never lead from outside the running-peer list.
		return undecided("own instance %s is not in the running-peer list", d.Self), nil
	case self.Draining:
		return undecided("draining"), nil
	}
	for _, name := range names {
		p := peers[name]
		if name != d.Self && p.Node == d.Node && !p.Draining && p.Generation < d.Generation {
			return undecided("older instance %s (generation %d) on this node leads", name, p.Generation), nil
		}
	}
	for _, name := range names {
		if name != d.Self && peers[name].Acting {
			return undecided("waiting for %s to stop acting", name), nil
		}
	}
	return Decision{Leader: true, Commit: des.Commit, Reason: fmt.Sprintf("designated primary at commit %s", short(des.Commit))}, nil
}

// MayUndrain reports whether an instance that drained itself for a
// recoverable reason — its git sync failing persistently (#187) — may clear
// its Draining flag now that sync has recovered.
//
// It may not if a higher-generation instance on its node is non-draining:
// that one took over while this one was drained (rule 4 let it), so this one
// has been superseded and must stay drained until the leader retires it. The
// check lives here, on the write side, rather than in MayAct, because MayAct's
// same-node rule is deliberately one-sided: an old instance keeps acting
// through a self-upgrade while its brand-new, non-draining candidate waits.
func MayUndrain(self, node string, generation int64, peers map[string]Peer) (bool, string) {
	for _, name := range slices.Sorted(maps.Keys(peers)) {
		p := peers[name]
		if name != self && p.Node == node && !p.Draining && p.Generation > generation {
			return false, fmt.Sprintf("superseded by %s (generation %d) on this node", name, p.Generation)
		}
	}
	return true, "no newer generation on this node took over"
}

// short abbreviates a commit hash for reasons and logs.
func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
