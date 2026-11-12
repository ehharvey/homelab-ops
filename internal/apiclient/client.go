// Package apiclient is the operator's client for the web app's HTTP API
// (#196, docs/Decisions.md §28): one method per route the API serves. It
// speaks HTTP over whatever *http.Client it is given; the transport, the
// operator's own WireGuard peer and the pinning TLS client over it
// (PinnedHTTPClient, #397), is built separately, so this file knows nothing
// of either.
//
// JSON routes return the server's body as is, so a caller can print it for
// jq. A non-2xx answer is an *APIError carrying the server's message. The
// image route returns its body as a stream (Download), never buffered, so a
// multi-GB image goes straight to a file or device, and a later
// decrypt-as-stream stage (#198) can wrap that reader.
package apiclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxJSONBody bounds what a JSON route may answer with. The largest, GET
// /instances, is a few KB per instance; the bound only stops a misbehaving
// server from filling memory.
const maxJSONBody = 32 << 20

// maxErrorBody bounds how much of an error answer becomes APIError.Message.
const maxErrorBody = 4 << 10

// Client calls the web app's API at a base URL, e.g. https://10.100.0.1, the
// API's address inside the tunnel.
type Client struct {
	base *url.URL
	http *http.Client
}

// New returns a Client for the API at baseURL, sending its requests with hc.
func New(baseURL string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse API base URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("API base URL %q: want http://host[:port] or https://host[:port]", baseURL)
	}
	if hc == nil {
		return nil, errors.New("apiclient: nil *http.Client")
	}
	return &Client{base: u, http: hc}, nil
}

// APIError is a non-2xx answer from the API. Message is the server's own
// text (its http.Error body), e.g. "sync failed" or "forbidden: not an
// operator peer".
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "(no message)"
	}
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode), msg)
}

// Status is GET /status: whether the web app has synced, and at which commit.
func (c *Client) Status(ctx context.Context) ([]byte, error) {
	return c.doJSON(ctx, http.MethodGet, "/status")
}

// Networks is GET /networks: the synced networks.
func (c *Client) Networks(ctx context.Context) ([]byte, error) {
	return c.doJSON(ctx, http.MethodGet, "/networks")
}

// Instances is GET /instances: the synced instances, with their assigned
// addresses.
func (c *Client) Instances(ctx context.Context) ([]byte, error) {
	return c.doJSON(ctx, http.MethodGet, "/instances")
}

// Sync is POST /sync: pull the config repo now. A failed pull or an invalid
// fleet is a 502 from the server, so an *APIError here.
func (c *Client) Sync(ctx context.Context) ([]byte, error) {
	return c.doJSON(ctx, http.MethodPost, "/sync")
}

// Seed is POST /instances/{name}/seed: the instance's rendered seed files,
// one YAML string per field. It holds the node's WireGuard private key and
// bootstrap credential, so save it as a secret.
func (c *Client) Seed(ctx context.Context, name string) ([]byte, error) {
	p, err := instancePath(name, "seed")
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodPost, p)
}

// DeployAgent is POST /instances/{name}/deploy-agent: put the first
// app-manager agent on the instance's node, over its tunnel.
func (c *Client) DeployAgent(ctx context.Context, name string) ([]byte, error) {
	p, err := instancePath(name, "deploy-agent")
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodPost, p)
}

// Download is a streamed response body. Size is its Content-Length, or -1
// when the server sent none. The caller must Close Body.
type Download struct {
	Body io.ReadCloser
	Size int64
}

// Image is GET /instances/{name}/image: the instance's freshly built,
// seeded disk image, as a stream. Nothing is read until the caller reads
// Body, so the image is never held in memory; ctx bounds the whole
// download, not just the request.
func (c *Client) Image(ctx context.Context, name string) (*Download, error) {
	p, err := instancePath(name, "image")
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, p) //nolint:bodyclose // the body is handed to the caller as Download.Body, which it must Close
	if err != nil {
		return nil, err
	}
	return &Download{Body: resp.Body, Size: resp.ContentLength}, nil
}

// CopyDownload copies d's whole body to w and checks it against d.Size, so
// a download cut short (the connection dropped, or the server gave up
// mid-stream) is an error rather than a silently truncated image.
func CopyDownload(w io.Writer, d *Download) (int64, error) {
	n, err := io.Copy(w, d.Body)
	if err != nil {
		return n, fmt.Errorf("download: %w", err)
	}
	if d.Size >= 0 && n != d.Size {
		return n, fmt.Errorf("download: got %d bytes, server announced %d", n, d.Size)
	}
	return n, nil
}

func instancePath(name, action string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/") {
		return "", fmt.Errorf("instance name %q: must be non-empty and contain no '/'", name)
	}
	return "/instances/" + url.PathEscape(name) + "/" + action, nil
}

// doJSON sends one request and returns the 2xx body, read whole.
func (c *Client) doJSON(ctx context.Context, method, path string) ([]byte, error) {
	resp, err := c.do(ctx, method, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response, nothing to flush
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if len(body) > maxJSONBody {
		return nil, fmt.Errorf("%s %s: response is over %d bytes", method, path, maxJSONBody)
	}
	return body, nil
}

// do sends one bodiless request. On a 2xx it returns the response with its
// body unread; otherwise it closes the body and returns an *APIError.
func (c *Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	// path is already escaped (instancePath), so it joins as text.
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.base.String(), "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%s %s: build request: %w", method, path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close() //nolint:errcheck // read-only response, nothing to flush
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Message:    string(bytes.TrimSpace(msg)),
		}
	}
	return resp, nil
}
