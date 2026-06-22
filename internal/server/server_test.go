package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

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

type fakeStore struct {
	replaceCfg    config.Config
	replaceCommit string
	replaceErr    error
	replaced      bool

	commit   string
	syncedAt time.Time
	synced   bool
	lastErr  error

	networks    []config.Network
	networksErr error
	instances   []config.Instance
	instErr     error
}

func (f *fakeStore) Replace(_ context.Context, cfg config.Config, commit string, _ time.Time) error {
	f.replaced = true
	f.replaceCfg = cfg
	f.replaceCommit = commit
	return f.replaceErr
}

func (f *fakeStore) LastSync(context.Context) (string, time.Time, bool, error) {
	return f.commit, f.syncedAt, f.synced, f.lastErr
}

func (f *fakeStore) Networks(context.Context) ([]config.Network, error) {
	return f.networks, f.networksErr
}

func (f *fakeStore) Instances(context.Context) ([]config.Instance, error) {
	return f.instances, f.instErr
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	New(nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSyncNotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /sync with nil syncer = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestSyncSuccess(t *testing.T) {
	cfg := config.Config{
		Networks:  []config.Network{{Name: "dev-lan"}},
		Instances: []config.Instance{{Name: "devnode0"}},
	}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}
	store := &fakeStore{}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store).ServeHTTP(rec, req)

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

	if !store.replaced {
		t.Fatalf("POST /sync did not call Store.Replace")
	}
	if store.replaceCommit != "deadbeef" || !reflect.DeepEqual(store.replaceCfg, cfg) {
		t.Errorf("Replace called with (%+v, %q), want (%+v, %q)", store.replaceCfg, store.replaceCommit, cfg, "deadbeef")
	}
}

func TestSyncFailure(t *testing.T) {
	syncer := fakeSyncer{err: errors.New("clone failed: secret-bearing-url")}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /sync with failing syncer = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if strings.Contains(rec.Body.String(), "secret-bearing-url") {
		t.Errorf("response body leaked internal error detail: %q", rec.Body.String())
	}
}

func TestSyncStoreFailure(t *testing.T) {
	syncer := fakeSyncer{cfg: config.Config{}, sha: "deadbeef"}
	store := &fakeStore{replaceErr: errors.New("disk full")}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /sync with failing store = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	New(nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /status with nil store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestStatusNeverSynced(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	New(nil, &fakeStore{synced: false}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Synced {
		t.Errorf("status = %+v, want synced=false", got)
	}
}

func TestStatusSynced(t *testing.T) {
	syncedAt := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{synced: true, commit: "deadbeef", syncedAt: syncedAt}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	New(nil, store).ServeHTTP(rec, req)

	var got statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := statusResponse{Synced: true, Commit: "deadbeef", SyncedAt: syncedAt.Format(time.RFC3339)}
	if got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

func TestNetworksAndInstances(t *testing.T) {
	store := &fakeStore{
		networks:  []config.Network{{Name: "dev-lan"}},
		instances: []config.Instance{{Name: "devnode0"}},
	}

	req := httptest.NewRequest(http.MethodGet, "/networks", nil)
	rec := httptest.NewRecorder()
	New(nil, store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /networks = %d, want %d", rec.Code, http.StatusOK)
	}
	var networks []config.Network
	if err := json.NewDecoder(rec.Body).Decode(&networks); err != nil {
		t.Fatalf("decode networks: %v", err)
	}
	if !reflect.DeepEqual(networks, store.networks) {
		t.Errorf("networks = %+v, want %+v", networks, store.networks)
	}

	req = httptest.NewRequest(http.MethodGet, "/instances", nil)
	rec = httptest.NewRecorder()
	New(nil, store).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /instances = %d, want %d", rec.Code, http.StatusOK)
	}
	var instances []config.Instance
	if err := json.NewDecoder(rec.Body).Decode(&instances); err != nil {
		t.Fatalf("decode instances: %v", err)
	}
	if !reflect.DeepEqual(instances, store.instances) {
		t.Errorf("instances = %+v, want %+v", instances, store.instances)
	}
}

func TestNetworksInstancesNotConfigured(t *testing.T) {
	for _, path := range []string{"/networks", "/instances"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		New(nil, nil).ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with nil store = %d, want %d", path, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/healthz"},
		{http.MethodGet, "/sync"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()

		New(nil, nil).ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}
