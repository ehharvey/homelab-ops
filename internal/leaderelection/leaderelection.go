// Package leaderelection decides whether this app-manager agent may act as
// the fleet's single leader right now. Every leader-only action (fleet
// reconciliation, per docs/AppManager.md) is gated on it.
//
// The mechanism is deliberately pluggable. Callers depend only on Elector;
// how "may act" is decided is an implementation detail behind it. Today's
// implementation is Designated: an operator names the primary in git, and
// agents fence a stale designation with a monotonic epoch. A later
// implementation can elect automatically (the "ranked over Incus" protocol
// specified in docs/Decisions.md §25) without touching the reconcile loop.
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
	// Epoch is the leadership epoch the decision was made under, or 0 if the
	// implementation has no such notion. Useful for logging and for tagging
	// what a leader created.
	Epoch int64
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
