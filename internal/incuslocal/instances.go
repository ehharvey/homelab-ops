package incuslocal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// AppTagKey marks an instance as one the app-manager agent owns, holding the
// name of the App it belongs to. ListInstances returns only instances
// carrying it, so nothing this package hands a caller is a workload the
// fleet does not manage.
const AppTagKey = "user.homelab-ops.app"

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
	// Status is Incus's own status string, e.g. "Running" or "Stopped".
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
			Name:     inst.Name,
			Status:   inst.Status,
			Location: inst.Location,
			Config:   inst.Config,
		})
	}
	return out, nil
}

// CreateInstance creates req and waits for the resulting operation to
// finish, so a returned nil means the instance exists rather than merely
// that Incus accepted the request.
func (c *Client) CreateInstance(ctx context.Context, req lxcapi.InstancesPost) error {
	if req.Name == "" {
		return fmt.Errorf("incuslocal: instance name is required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal create request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/1.0/instances", bytes.NewReader(body))
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

// DeleteInstance deletes name and waits for the operation to finish.
//
// Incus refuses to delete a running instance, so callers that mean "remove
// this generation" stop it first; this package does not stop it for them,
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
// the single-writer premise, and concurrent writers will lose updates.
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
