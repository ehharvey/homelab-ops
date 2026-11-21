package apiclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"

	"filippo.io/age"
)

// A request that never reached the web app (the dial failed) is ErrNotSent,
// so a key verb can say its new key wasn't taken; one the web app read and
// then dropped without answering may have been acted on, so it isn't.
func TestErrNotSentOnlyBeforeTheRequestIsWritten(t *testing.T) {
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	refused := errors.New("connection refused")
	c, err := New("http://10.100.0.1", HTTPClient(func(context.Context, string, string) (net.Conn, error) {
		return nil, refused
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Init(context.Background(), key); !errors.Is(err, ErrNotSent) || !errors.Is(err, refused) {
		t.Errorf("Init with the dial failing = %v, want ErrNotSent wrapping %v", err, refused)
	}

	dropped := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	if _, err := dropped.Init(context.Background(), key); err == nil || errors.Is(err, ErrNotSent) {
		t.Errorf("Init whose answer was dropped = %v, want an error that isn't ErrNotSent", err)
	}

	if NotSent(nil) != nil {
		t.Error("NotSent(nil) isn't nil")
	}
}
