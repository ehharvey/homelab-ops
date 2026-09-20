package leaderelection

import (
	"context"
	"errors"
	"fmt"
)

// Designation is the operator's statement of who leads: a primary node and a
// monotonic epoch. It comes from git config, which the agent syncs each tick;
// where it is parsed from is the caller's concern (#101), not this package's.
//
// Failing over means the operator fences the old primary (stops or deletes its
// agent instance) and then raises Epoch while naming a new Primary. The epoch
// is what stops a stale git checkout from being obeyed: see Designated.
type Designation struct {
	Primary string
	Epoch   int64
}

// Peer is what one agent instance publishes about itself. Each field is
// written by that instance alone (a user.* key on its own Incus instance in the
// Incus-backed implementation, #101), so no compare-and-swap is needed.
type Peer struct {
	// Node is the node the instance runs on, matched against
	// Designation.Primary.
	Node string
	// Generation is the instance's blue-green generation on that node. During a
	// self-upgrade the old and the candidate instance both run there; the
	// oldest live, non-draining one leads.
	Generation int64
	// Epoch is the highest designation epoch the instance has recorded.
	Epoch int64
	// Draining means the instance has stopped acting (a leader that has seen
	// its own image go stale and a sustained-healthy candidate sets it, #109,
	// so the candidate can take over and retire it).
	Draining bool
}

// Registry is the shared record agents publish to and read from.
type Registry interface {
	// Record raises this agent's recorded epoch to at least epoch. It must
	// never lower it: a ratchet, so an agent that once saw epoch N can't be
	// talked back into N-1 by a stale sync.
	Record(ctx context.Context, epoch int64) error
	// Peers returns the published state of every running agent, keyed by
	// instance name, including this agent's own. A stopped or deleted instance
	// must not appear: a dead older generation would otherwise block its
	// candidate from ever leading.
	Peers(ctx context.Context) (map[string]Peer, error)
}

// Designated is the Elector for operator-designated leadership.
//
// An agent may act iff, all at once:
//   - it runs on the designated Primary node;
//   - no agent anywhere has recorded a higher epoch than the designation
//     carries (so a stale git checkout is not obeyed); and
//   - it is not draining, and no older non-draining instance shares its node
//     (so during a blue-green self-upgrade exactly one of the old and candidate
//     instances acts, and the old one is never the one to retire itself).
//
// Every agent records the epoch it sees, primary or not, so an old primary
// running on a stale git checkout learns of a newer designation the moment any
// peer has seen it and stands down.
//
// This is a deterministic function of its inputs and holds no timers. It
// guarantees a single writer given the operator's part of the protocol —
// fence the old primary before raising the epoch — because it doesn't try to
// detect failure or pick a replacement itself; that's the trade docs/
// Decisions.md §25 makes for 0.x.
type Designated struct {
	// Self is this agent's instance name, its key in Registry.Peers.
	Self string
	// Node and Generation identify where and which generation this agent is.
	Node       string
	Generation int64
	// Source returns the current designation from the agent's git sync.
	Source func() (Designation, error)
	// Peers is the shared record.
	Peers Registry
}

var _ Elector = (*Designated)(nil)

// MayAct implements Elector.
func (d *Designated) MayAct(ctx context.Context) (Decision, error) {
	if d.Self == "" || d.Node == "" {
		return Decision{}, errors.New("leaderelection: Designated.Self and Node must be set")
	}
	des, err := d.Source()
	if err != nil {
		return Decision{}, fmt.Errorf("leaderelection: reading designation: %w", err)
	}
	if des.Primary == "" || des.Epoch < 1 {
		return Decision{Reason: "no primary designated"}, nil
	}

	// Record before reading: publish-then-read is what lets two agents that
	// disagree about the epoch find out about each other.
	if err := d.Peers.Record(ctx, des.Epoch); err != nil {
		return Decision{}, fmt.Errorf("leaderelection: recording epoch %d: %w", des.Epoch, err)
	}
	peers, err := d.Peers.Peers(ctx)
	if err != nil {
		return Decision{}, fmt.Errorf("leaderelection: reading peers: %w", err)
	}
	for name, p := range peers {
		if p.Epoch > des.Epoch {
			return Decision{
				Epoch:  des.Epoch,
				Reason: fmt.Sprintf("designation epoch %d is stale: %s has recorded epoch %d", des.Epoch, name, p.Epoch),
			}, nil
		}
	}

	if des.Primary != d.Node {
		return Decision{Epoch: des.Epoch, Reason: fmt.Sprintf("designated primary is %s", des.Primary)}, nil
	}
	if peers[d.Self].Draining {
		return Decision{Epoch: des.Epoch, Reason: "draining: a candidate is taking over"}, nil
	}
	for name, p := range peers {
		if name != d.Self && p.Node == d.Node && !p.Draining && p.Generation < d.Generation {
			return Decision{Epoch: des.Epoch, Reason: fmt.Sprintf("older instance %s (generation %d) on this node leads", name, p.Generation)}, nil
		}
	}
	return Decision{Leader: true, Epoch: des.Epoch, Reason: fmt.Sprintf("designated primary at epoch %d", des.Epoch)}, nil
}
