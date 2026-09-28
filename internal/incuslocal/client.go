// Package incuslocal is an Incus API client over the local host's unix
// socket, used by the per-node app-manager agent (#92) to reconcile
// instances on the node it runs on.
//
// It is deliberately a near-duplicate of internal/nodeprovision's
// create/wait/decode shape rather than a shared abstraction over it: the two
// have genuinely different transports (TLS-over-WireGuard-tunnel dial there,
// plain unix socket and no TLS at all here) and there are only two consumers.
// docs/Decisions.md §16 settled that trade and names the revisit trigger — a
// third Incus-API-consuming package, concretely #77's Alloy renderer.
//
// Same judgment call nodeprovision already established: a plain *http.Client
// against lxc/incus/v7/shared/api types directly, rather than pulling in
// lxc/incus/v7/client and its dependency tree.
//
// # No project support
//
// Every call targets Incus's default project, like internal/nodeprovision
// before it. 0.x puts the agent's instances there, so a project parameter
// would be a knob with one possible value. Add one when something actually
// needs a second project — the coordination project the superseded lease
// design wanted (docs/Decisions.md §17) is not it, since §25 replaced that
// with git-declared designation.
//
// # No conditional writes
//
// This package exposes no ETag/If-Match write, on purpose. #160's spike
// (scripts/validate/incus-etag-write-guards-lost-updates-not-races.sh)
// measured that Incus's If-Match is a lost-update guard, not a
// compare-and-swap: with 16 concurrent writers on one ETag, ~15% of rounds
// admitted two winners, because the check and the write are not atomic. So
// nothing here should be built on it. Leadership instead comes from an
// operator's git-declared designation with epoch fencing
// (internal/leaderelection.Designated, #108), which contends for nothing.
//
// SetUserKeys is consequently unconditional, and safe only because of a
// property its callers hold rather than anything this package enforces: an
// agent writes its own instance's keys and no other. See its doc comment.
package incuslocal

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// baseURL is the scheme+host every request is built against. The host is
// meaningless over a unix socket — the transport below ignores the address it
// is handed and always dials the configured socket path — but net/http still
// requires a syntactically valid absolute URL.
const baseURL = "http://unix"

// Client talks to one Incus daemon over its unix socket.
//
// Safe for concurrent use: it holds only an *http.Client.
type Client struct {
	http *http.Client
}

// Dial returns a Client that reaches Incus over socketPath.
//
// The path is checked to exist and to be a socket, so a typo or a
// wrong-host mistake fails here rather than surfacing later as a confusing
// per-request dial error. That check is not a completed connection: unix
// dials happen per request, and a socket nothing is listening on still
// passes. Callers that need liveness should make a real call.
func Dial(socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("incuslocal: socket path is required")
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("incuslocal: stat socket %s: %w", socketPath, err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return nil, fmt.Errorf("incuslocal: %s is not a socket (mode %s)", socketPath, info.Mode())
	}

	return &Client{http: &http.Client{
		Transport: &http.Transport{
			// The network and address http hands us come from baseURL and
			// are both discarded: every request goes to the one socket.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		// No Timeout: every method takes a context and callers bound it
		// there, matching internal/nodeprovision.
	}}, nil
}

// do sends req and decodes Incus's standard response envelope, mapping an
// "error"-typed envelope (a well-formed Incus error, as opposed to a
// transport failure) to a Go error. Mirrors internal/nodeprovision.do.
func (c *Client) do(req *http.Request) (*lxcapi.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response, nothing to flush

	var envelope lxcapi.Response
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode response (http %d): %w", resp.StatusCode, err)
	}
	if envelope.Type == lxcapi.ErrorResponse {
		return nil, fmt.Errorf("incus error: %s", envelope.Error)
	}
	return &envelope, nil
}

// waitOperation blocks until the operation named by operationID finishes,
// and reports a failed operation as an error.
//
// Incus answers an async request with "Operation created" before the work
// has happened, so a successful POST proves nothing about the outcome — the
// real error only surfaces here. That is not hypothetical: #161 was exactly
// this, a create that returned an operation and then failed on a storage
// pool that did not exist.
func (c *Client) waitOperation(ctx context.Context, operationID string) error {
	url := fmt.Sprintf("%s/1.0/operations/%s/wait", baseURL, operationID)
	// If ctx carries a deadline, tell Incus about it too via ?timeout=
	// (seconds), so the server-side wait and our own deadline stay aligned
	// rather than relying solely on client-side cancellation to unblock.
	// Same reasoning as internal/nodeprovision.waitOperation.
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			url += fmt.Sprintf("?timeout=%d", int(remaining.Seconds()))
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build wait request: %w", err)
	}

	envelope, err := c.do(req)
	if err != nil {
		return err
	}

	var final lxcapi.Operation
	if err := json.Unmarshal(envelope.Metadata, &final); err != nil {
		return fmt.Errorf("decode operation: %w", err)
	}
	if final.StatusCode == lxcapi.Failure {
		return fmt.Errorf("operation failed: %s", final.Err)
	}
	return nil
}

// awaitEnvelope waits on envelope's operation when it is async, and returns
// immediately when it is synchronous.
//
// Which one Incus sends is not always predictable from the request: a
// config-only PATCH against a never-started instance answers synchronously
// (measured by #160's spike), while the same call against a running one may
// hand back an operation. Rather than assume the shape the spike happened
// to observe, both are handled.
func (c *Client) awaitEnvelope(ctx context.Context, envelope *lxcapi.Response) error {
	if envelope.Type != lxcapi.AsyncResponse {
		return nil
	}
	var op lxcapi.Operation
	if err := json.Unmarshal(envelope.Metadata, &op); err != nil {
		return fmt.Errorf("decode operation: %w", err)
	}
	return c.waitOperation(ctx, op.ID)
}
