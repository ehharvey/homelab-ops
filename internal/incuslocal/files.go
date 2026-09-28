package incuslocal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// maxFileBytes caps what ReadFile will read into memory. Its callers read
// small agent-written control files (a heartbeat timestamp), not payloads, so
// a bound costs nothing and stops a wrong path — /dev/zero, a large log —
// from exhausting the agent's memory.
const maxFileBytes = 1 << 20 // 1 MiB

// ReadFile returns the contents of path inside instance name.
//
// This is GET /1.0/instances/{name}/files?path=..., which answers with the
// file's bytes as the response body rather than Incus's usual JSON envelope —
// so, unlike every other method here, it does not go through do().
//
// It exists for docs/AppManager.md's Healthy check, which is defined as "the
// freshness of a heartbeat file the agent's own process writes on every
// tick". Reading a file is deliberately not done via instance exec: exec is
// websocket-first, and its websocket-free form (RecordOutput) writes the
// output to log files that then have to be fetched and explicitly deleted —
// upstream's own client does POST, wait, GET, DELETE per call and treats a
// failed delete as fatal. For reading a file on a path that runs every tick,
// a single GET is the right primitive.
//
// A missing file is an error, never empty content: a heartbeat check that
// read "absent" as "fine" would invert the very thing it tests.
func (c *Client) ReadFile(ctx context.Context, name, path string) ([]byte, error) {
	if name == "" || path == "" {
		return nil, fmt.Errorf("incuslocal: instance name and file path are required")
	}

	query := url.Values{"path": []string{path}}
	reqURL := baseURL + "/1.0/instances/" + url.PathEscape(name) + "/files?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build read-file request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s from %q: %w", path, name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response, nothing to flush

	// A non-200 body is an ordinary Incus error envelope, so prefer its
	// message ("not found" and the like) over the bare status code.
	if resp.StatusCode != http.StatusOK {
		var envelope lxcapi.Response
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err == nil && envelope.Error != "" {
			return nil, fmt.Errorf("read %s from %q: incus error (http %d): %s", path, name, resp.StatusCode, envelope.Error)
		}
		return nil, fmt.Errorf("read %s from %q: unexpected http %d", path, name, resp.StatusCode)
	}

	// Incus signals a directory through X-Incus-type, and then sends a JSON
	// listing rather than file bytes. Returning that listing as though it
	// were file content would be a silently wrong answer.
	if _, _, _, fileType, _ := lxcapi.ParseFileHeaders(resp.Header); fileType == "directory" {
		return nil, fmt.Errorf("read %s from %q: is a directory, not a file", path, name)
	}

	// LimitReader at the cap plus one byte, so hitting the cap is
	// distinguishable from a file that is exactly maxFileBytes long.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s from %q: %w", path, name, err)
	}
	if len(body) > maxFileBytes {
		return nil, fmt.Errorf("read %s from %q: larger than the %d-byte limit", path, name, maxFileBytes)
	}
	return body, nil
}
