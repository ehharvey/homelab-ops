package incuslocal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// AppTagKey marks an instance as one the app-manager agent owns, holding the
// name of the App it belongs to. ListInstances returns only instances
// carrying it, so nothing this package hands a caller is a workload the
// fleet does not manage.
const AppTagKey = "user.homelab-ops.app"

// GenerationTagKey holds an App instance's blue-green generation as a
// decimal string ("0" for the first). #98's reconcile loop keys its state
// machine on it; #203's agent creates write "0".
const GenerationTagKey = "user.homelab-ops.generation"

// ImageTagKey holds the image an App instance was created from, as the App's
// declared image serialized by agentdeploy.ImageTag (docs/AppManager.md).
// Comparing it with the declared image is how an image change is detected
// (#109, and #98 in Phase 8).
const ImageTagKey = "user.homelab-ops.image"

// StatusRunning is the Status of an instance that is up. Incus's others
// include "Stopped", "Frozen" and "Error".
const StatusRunning = "Running"

// UnclusteredLocation is the Location Incus reports for every instance on an
// unclustered daemon (see Instance.Location).
const UnclusteredLocation = "none"

// userKeyPrefix is the only config namespace SetUserKeys will write. Incus
// reserves user.* for exactly this: arbitrary caller metadata it never
// interprets.
const userKeyPrefix = "user."

// Instance is one managed instance as ListInstances sees it.
//
// The fields are chosen so that #98's reconcile loop and #101's
// leaderelection.Registry can each decide everything from a single
// ListInstances call — docs/AppManager.md's central property is that state is
// "re-derivable from incus list alone, nothing held in the reconciler's own
// memory", which only holds if one listing carries enough.
type Instance struct {
	// Name is the instance name, e.g. "agent-node0-g3".
	Name string
	// Status is Incus's own status string, e.g. StatusRunning or "Stopped".
	//
	// Load-bearing, not informational: leaderelection.Registry.Peers must
	// exclude anything not running, because a dead older generation that
	// still appeared would block its candidate from ever leading.
	Status string
	// Location is the cluster member the instance sits on, passed through
	// from Incus verbatim. On a clustered daemon that is the member name
	// ("pc0"); on an unclustered one Incus reports the literal string
	// "none", NOT an empty string — checked first-hand against both, because
	// `if inst.Location == ""` is the obvious wrong guess and it never fires.
	// One socket reaches every member of a clustered Incus, so this is how a
	// caller tells them apart.
	Location string
	// Config is the instance's configuration exactly as Incus returned it,
	// unfiltered — callers read their own user.homelab-ops.* keys out of it.
	// Left whole rather than narrowed to user.* so that nothing is silently
	// unavailable to a later caller.
	Config map[string]string
	// CreatedAt is when Incus created the instance, as it reports it. The
	// self-upgrade's health deadline counts from it (#109), so the deadline
	// is re-derived from the listing rather than held in the agent's memory.
	CreatedAt time.Time
}

// ListInstances returns every instance carrying AppTagKey.
//
// Filtering happens here, in Go, rather than through Incus's own ?filter=
// query parameter, which could express it (the syntax is "<field> eq
// <value>"). That is a deliberate safety choice, not an oversight: the
// reconcile loop's zero-match branch *creates* (docs/AppManager.md — "How
// many? -> zero -> Create generation 0"), so a subtly wrong filter
// expression returns an empty list, which is indistinguishable from "this
// App has no instances yet", and the symptom is spurious instance creation
// rather than anything that looks like a filter bug. A Go predicate is
// covered by unit tests instead. Worth revisiting if a node ever runs enough
// instances that recursion=1 per tick is a measurable payload cost.
func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/1.0/instances?recursion=1", nil)
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}

	envelope, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}

	var raw []lxcapi.Instance
	if err := json.Unmarshal(envelope.Metadata, &raw); err != nil {
		return nil, fmt.Errorf("decode instances: %w", err)
	}

	out := make([]Instance, 0, len(raw))
	for _, inst := range raw {
		if inst.Config[AppTagKey] == "" {
			continue
		}
		out = append(out, Instance{
			Name:      inst.Name,
			Status:    inst.Status,
			Location:  inst.Location,
			Config:    inst.Config,
			CreatedAt: inst.CreatedAt,
		})
	}
	return out, nil
}

// CreateInstance creates req and waits for the resulting operation to
// finish, so a returned nil means the instance exists rather than merely
// that Incus accepted the request.
//
// A non-empty target places the instance on that cluster member
// (POST /1.0/instances?target=<member>). An empty one leaves placement to
// Incus: on a standalone daemon that is the daemon itself, and an
// unclustered daemon rejects any target, so a caller passes one only for a
// member it read from ListMembers.
func (c *Client) CreateInstance(ctx context.Context, req lxcapi.InstancesPost, target string) error {
	if req.Name == "" {
		return fmt.Errorf("incuslocal: instance name is required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal create request: %w", err)
	}

	u := baseURL + "/1.0/instances"
	if target != "" {
		u += "?" + url.Values{"target": {target}}.Encode()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	envelope, err := c.do(httpReq)
	if err != nil {
		return fmt.Errorf("create instance %q: %w", req.Name, err)
	}
	if err := c.awaitEnvelope(ctx, envelope); err != nil {
		return fmt.Errorf("create instance %q: %w", req.Name, err)
	}
	return nil
}

// StopInstance force-stops name (PUT /1.0/instances/<name>/state, action
// stop) and waits for the operation to finish. Incus refuses to stop an
// instance that isn't running, so callers stop only what the listing shows
// Running.
//
// The stop is forced, not a clean shutdown: its caller (#109's self-upgrade)
// stops only an instance that isn't acting, a non-acting candidate or an old
// generation that has published acting: false, so there is no leader work in
// it to wind down.
func (c *Client) StopInstance(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("incuslocal: instance name is required")
	}

	body, err := json.Marshal(lxcapi.InstanceStatePut{Action: "stop", Force: true, Timeout: -1})
	if err != nil {
		return fmt.Errorf("marshal stop request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+"/1.0/instances/"+url.PathEscape(name)+"/state", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build stop request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	envelope, err := c.do(req)
	if err != nil {
		return fmt.Errorf("stop instance %q: %w", name, err)
	}
	if err := c.awaitEnvelope(ctx, envelope); err != nil {
		return fmt.Errorf("stop instance %q: %w", name, err)
	}
	return nil
}

// DeleteInstance deletes name and waits for the operation to finish.
//
// Incus refuses to delete a running instance, so callers that mean "remove
// this generation" stop it first (StopInstance); this package does not stop
// it for them,
// because the reconcile loop's self-recognition rule (docs/AppManager.md)
// makes "which instance may be destroyed" a decision that must stay with
// the caller.
func (c *Client) DeleteInstance(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("incuslocal: instance name is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/1.0/instances/"+url.PathEscape(name), nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}

	envelope, err := c.do(req)
	if err != nil {
		return fmt.Errorf("delete instance %q: %w", name, err)
	}
	if err := c.awaitEnvelope(ctx, envelope); err != nil {
		return fmt.Errorf("delete instance %q: %w", name, err)
	}
	return nil
}

// SetUserKeys merges keys into name's configuration, leaving every other
// config key untouched (PATCH semantics, not PUT).
//
// Every key must be in the user.* namespace; anything else is rejected
// before the request is sent. That guardrail keeps this from becoming a
// general-purpose config writer able to change limits or devices, which is
// not what it is for.
//
// # Why there is no precondition
//
// This is an unconditional PATCH: no ETag, no If-Match. It is safe only
// because of a property the *caller* holds — an agent writes its own
// instance's keys and nobody else's, so there is exactly one writer per
// instance and no race to lose. Nothing here enforces that.
//
// Two consequences worth stating plainly, because the absence of a
// precondition is otherwise the sort of thing a later reader "fixes":
//
//   - Adding an If-Match would not help. #160's spike measured that Incus's
//     conditional write is a lost-update guard and not a compare-and-swap:
//     the check and the write are not atomic, and under contention two
//     writers both succeed (~15% of rounds with 16 writers). It would buy
//     the appearance of safety, not safety.
//   - Read-modify-write logic, if a caller ever needs one, belongs above
//     this package, where the single-writer argument is visible. If it were
//     expressed here it would read as an RMW wanting a compare-and-swap, and
//     the justification for having none would sit nowhere near it. (The
//     epoch ratchet that was the motivating case went away with #212; the
//     Incus-backed leaderelection.Registry, internal/incusregistry, now only
//     ever writes whole values.)
//
// Using this method against an instance other than the caller's own breaks
// the single-writer premise, and concurrent writers will lose updates. The
// one exception keeps it per key: the acting primary's self-upgrade (#109)
// writes its own bookkeeping keys (internal/agent's KeyHealthySince,
// KeyUpgradeAttempts, KeyUpgradeImage) on other agents' instances. Only the
// acting primary writes them, at most one agent acts at a time, and a PATCH
// leaves the instance's own keys alone, so each key still has one writer.
func (c *Client) SetUserKeys(ctx context.Context, name string, keys map[string]string) error {
	if name == "" {
		return fmt.Errorf("incuslocal: instance name is required")
	}
	if len(keys) == 0 {
		return fmt.Errorf("incuslocal: no keys to set on %q", name)
	}
	for k := range keys {
		if !strings.HasPrefix(k, userKeyPrefix) {
			return fmt.Errorf("incuslocal: refusing to set non-user key %q on %q", k, name)
		}
	}

	body, err := json.Marshal(lxcapi.InstancePut{Config: keys})
	if err != nil {
		return fmt.Errorf("marshal config patch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, baseURL+"/1.0/instances/"+url.PathEscape(name), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	envelope, err := c.do(req)
	if err != nil {
		return fmt.Errorf("set user keys on %q: %w", name, err)
	}
	if err := c.awaitEnvelope(ctx, envelope); err != nil {
		return fmt.Errorf("set user keys on %q: %w", name, err)
	}
	return nil
}
