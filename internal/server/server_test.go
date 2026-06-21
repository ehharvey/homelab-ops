package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

type fakeSyncer struct {
	cfg config.Config
	sha string
	err error
}

func (f fakeSyncer) Sync(context.Context) (config.Config, string, error) {
	return f.cfg, f.sha, f.err
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	New(nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSyncNotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /sync with nil syncer = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestSyncSuccess(t *testing.T) {
	syncer := fakeSyncer{
		cfg: config.Config{
			Networks:  []config.Network{{Name: "dev-lan"}},
			Instances: []config.Instance{{Name: "devnode0"}},
		},
		sha: "deadbeef",
	}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sync = %d, want %d", rec.Code, http.StatusOK)
	}

	var got syncResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := syncResponse{Commit: "deadbeef", Networks: 1, Instances: 1}
	if got != want {
		t.Errorf("response = %+v, want %+v", got, want)
	}
}

func TestSyncFailure(t *testing.T) {
	syncer := fakeSyncer{err: errors.New("clone failed")}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /sync with failing syncer = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}
