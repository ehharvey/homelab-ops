// Package leaderelection decides whether this app-manager agent may act as
// the fleet's single leader right now. Every leader-only action (fleet
// reconciliation, per docs/AppManager.md) is gated on it.
//
// The mechanism is deliberately pluggable. Callers depend only on Elector;
// how "may act" is decided is an implementation detail behind it. Today's
// implementation is Designated: an operator names the primary node in git,
// agents fence a stale checkout with the git commit each one publishes, and a
// new primary waits for the old one to publish that it has stopped acting
// (docs/Decisions.md §25 and its #212 addendum). A later implementation can
// elect automatically (the "ranked over Incus" protocol specified in §25)
// without touching the reconcile loop.
//
// Gating isn't the whole fence. Across a network partition, the isolated
// agent stops only because its Registry reads fail without Incus quorum, and
// its work stops only if that work can't complete without quorum either. So
// every leader-only action must be an Incus API call through the agent's own
// member; anything else needs a fence of its own first (docs/Decisions.md
// §25, the guarantee's conditions).
//
// Why not an Incus-native lease: Incus's ETag/If-Match is a lost-update
// guard, not a compare-and-swap — under contention it admits more than one
// winner (measured in #160). See docs/Decisions.md §25 for that evidence and
// every alternative considered.
package leaderelection

import "context"

// Decision is the answer to "may this agent act as leader right now?".
type Decision struct {
	// Leader reports whether this agent may perform leader-only actions.
	Leader bool
	// Commit is the git commit the decision's designation was read at, or ""
	// if the implementation has no such notion or never got that far. Useful
	// for logging and for tagging what a leader created.
	Commit string
	// Behind reports that a running peer has published a commit this agent
	// doesn't have: its checkout is stale. The caller should re-sync now
	// rather than wait for its next poll (docs/Decisions.md §25, #212).
	Behind bool
	// Reason is a short human-readable explanation, for logs and the web
	// app's status display. Set whether or not Leader is true.
	Reason string
}

// Elector answers whether this agent may act as leader.
//
// MayAct must be cheap enough to call before every leader-only action, and
// callers should: "am I leader" is always re-derived, never cached, so a
// process that was paused or partitioned can't keep acting on a stale answer.
// A non-nil error means the answer is unknown; callers must treat that as
// "not leader".
type Elector interface {
	MayAct(ctx context.Context) (Decision, error)
}
