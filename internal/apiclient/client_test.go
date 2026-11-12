package apiclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestClient serves h over httptest and returns a Client for it.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestJSONRoutesHitTheirMethodAndPath(t *testing.T) {
	type call func(context.Context, *Client) ([]byte, error)
	cases := []struct {
		name, method, path string
		call               call
	}{
		{"status", http.MethodGet, "/status", func(ctx context.Context, c *Client) ([]byte, error) { return c.Status(ctx) }},
		{"networks", http.MethodGet, "/networks", func(ctx context.Context, c *Client) ([]byte, error) { return c.Networks(ctx) }},
		{"instances", http.MethodGet, "/instances", func(ctx context.Context, c *Client) ([]byte, error) { return c.Instances(ctx) }},
		{"sync", http.MethodPost, "/sync", func(ctx context.Context, c *Client) ([]byte, error) { return c.Sync(ctx) }},
		{"seed", http.MethodPost, "/instances/node0/seed", func(ctx context.Context, c *Client) ([]byte, error) { return c.Seed(ctx, "node0") }},
		{"deploy-agent", http.MethodPost, "/instances/node0/deploy-agent", func(ctx context.Context, c *Client) ([]byte, error) {
			return c.DeployAgent(ctx, "node0")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := `{"route":"` + tc.name + `"}` + "\n"
			mux := http.NewServeMux()
			mux.HandleFunc(tc.method+" "+tc.path, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, want)
			})
			got, err := tc.call(context.Background(), newTestClient(t, mux))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if string(got) != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

func TestErrorAnswerIsAPIErrorWithServerMessage(t *testing.T) {
	cases := []struct {
		status int
		msg    string
	}{
		{http.StatusBadGateway, "sync failed"},
		{http.StatusForbidden, "forbidden: not an operator peer"},
		{http.StatusServiceUnavailable, "image generation not configured (set BASE_IMAGE_PATH)"},
		{http.StatusNotFound, "404 page not found"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, tc.msg, tc.status)
			}))
			_, err := c.Sync(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Message != tc.msg {
				t.Errorf("APIError = %+v, want status %d message %q", apiErr, tc.status, tc.msg)
			}
			if !strings.Contains(err.Error(), tc.msg) || !strings.Contains(err.Error(), "POST /sync") {
				t.Errorf("Error() = %q, want the route and the server's message", err.Error())
			}
		})
	}
}

func TestImageErrorIsAPIError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "image generation not configured (set BASE_IMAGE_PATH)", http.StatusServiceUnavailable)
	}))
	_, err := c.Image(context.Background(), "node0")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 *APIError", err)
	}
}

// patternReader yields n bytes of a repeating pattern without allocating
// them, standing in for a multi-GB image.
type patternReader struct{ left int64 }

func (r *patternReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	for i := range p {
		p[i] = byte(i)
	}
	r.left -= int64(len(p))
	return len(p), nil
}

// chunkWriter records the largest single write it sees and counts the rest.
type chunkWriter struct {
	n, maxWrite int64
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	if int64(len(p)) > w.maxWrite {
		w.maxWrite = int64(len(p))
	}
	return len(p), nil
}

// TestImageStreamsWithoutBuffering serves a 64 MiB image and checks it
// arrives in small writes, so the client never held it whole.
func TestImageStreamsWithoutBuffering(t *testing.T) {
	const size = 64 << 20
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/instances/node0/image" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = io.Copy(w, &patternReader{left: size})
	}))
	dl, err := c.Image(context.Background(), "node0")
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	defer dl.Body.Close() //nolint:errcheck // test
	if dl.Size != size {
		t.Errorf("Size = %d, want %d", dl.Size, size)
	}
	var w chunkWriter
	n, err := CopyDownload(&w, dl)
	if err != nil {
		t.Fatalf("CopyDownload: %v", err)
	}
	if n != size || w.n != size {
		t.Errorf("copied %d (writer saw %d), want %d", n, w.n, size)
	}
	if w.maxWrite > 1<<20 {
		t.Errorf("largest write %d bytes: the body was buffered, not streamed", w.maxWrite)
	}
}

func TestCopyDownloadRejectsShortBody(t *testing.T) {
	dl := &Download{Body: io.NopCloser(bytes.NewReader(make([]byte, 10))), Size: 20}
	if _, err := CopyDownload(io.Discard, dl); err == nil || !strings.Contains(err.Error(), "announced 20") {
		t.Errorf("err = %v, want a size mismatch", err)
	}
}

func TestImageCutShortIsAnError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write(make([]byte, 100))
		// Returning with less than Content-Length written aborts the
		// connection, as a server dying mid-stream would.
	}))
	dl, err := c.Image(context.Background(), "node0")
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	defer dl.Body.Close() //nolint:errcheck // test
	if _, err := CopyDownload(io.Discard, dl); err == nil {
		t.Error("CopyDownload of a truncated body succeeded")
	}
}

func TestInstanceNameIsValidatedAndEscaped(t *testing.T) {
	var gotPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = io.WriteString(w, "{}")
	}))
	for _, bad := range []string{"", "a/b"} {
		if _, err := c.Seed(context.Background(), bad); err == nil {
			t.Errorf("Seed(%q) succeeded, want an error", bad)
		}
	}
	if _, err := c.Seed(context.Background(), "a b"); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if gotPath != "/instances/a%20b/seed" {
		t.Errorf("path = %q, want the name escaped", gotPath)
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	for _, u := range []string{"10.100.0.1", "ftp://x", "http://"} {
		if _, err := New(u, http.DefaultClient); err == nil {
			t.Errorf("New(%q) succeeded", u)
		}
	}
	if _, err := New("http://10.100.0.1", nil); err == nil {
		t.Error("New with a nil client succeeded")
	}
}

func TestBaseURLPathIsKept(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()
	c, err := New(srv.URL+"/api/", srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if gotPath != "/api/status" {
		t.Errorf("path = %q, want /api/status", gotPath)
	}
}

func TestRetryDialWaitsForAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test
	var attempts int
	var d net.Dialer
	flaky := func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("handshake not done")
		}
		return d.DialContext(ctx, network, address)
	}
	conn, err := RetryDial(flaky, 5*time.Second, time.Millisecond)(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("RetryDial: %v", err)
	}
	_ = conn.Close()
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestRetryDialGivesUpAfterTimeout(t *testing.T) {
	never := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	_, err := RetryDial(never, 50*time.Millisecond, 10*time.Millisecond)(context.Background(), "tcp", "10.100.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "no answer from 10.100.0.1:80 within 50ms") {
		t.Errorf("err = %v, want a no-answer error naming the address and timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("RetryDial took %s, want about the timeout", time.Since(start))
	}
}
