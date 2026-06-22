package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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
	// fakeStore{} has never synced (LastSync.ok == false), so this is a
	// first sync: no prior state to diff against, no warnings expected.
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

func TestSyncWithDiff(t *testing.T) {
	store := &fakeStore{
		synced:    true, // a prior sync happened, so this is not a "first sync"
		networks:  []config.Network{{Name: "dev-lan", CIDR: "10.0.0.0/24"}},
		instances: []config.Instance{{Name: "devnode0"}},
	}
	cfg := config.Config{
		Networks: []config.Network{
			{Name: "dev-lan", CIDR: "10.0.1.0/24"}, // changed
			{Name: "new-lan", CIDR: "10.0.2.0/24"}, // added
		},
		// devnode0 removed, no instances in the new sync
	}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}

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
	want := diffCounts{NetworksAdded: 1, NetworksChanged: 1, InstancesRemoved: 1}
	if got.Diff != want {
		t.Errorf("Diff = %+v, want %+v", got.Diff, want)
	}
	if !store.replaced {
		t.Fatalf("POST /sync did not call Store.Replace despite a diff read")
	}
}

func TestSyncDiffReadFailure(t *testing.T) {
	store := &fakeStore{synced: true, networksErr: errors.New("disk full")}
	cfg := config.Config{Networks: []config.Network{{Name: "dev-lan"}}}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store).ServeHTTP(rec, req)

	// A failure reading prior state for the diff must not fail the sync.
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sync with failing diff read = %d, want %d", rec.Code, http.StatusOK)
	}
	if !store.replaced {
		t.Fatalf("POST /sync did not call Store.Replace despite a failed diff read")
	}

	var got syncResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Diff != (diffCounts{}) {
		t.Errorf("Diff = %+v, want zero value", got.Diff)
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

// SyncOnce is the routine shared by POST /sync and cmd/web's background
// poller. The handler tests above exercise it indirectly; these cover it
// directly, including the diff it returns to the poller (which the HTTP
// response only ever exposes as counts) and its error classification.

func TestSyncOnceReturnsDiffToCaller(t *testing.T) {
	store := &fakeStore{
		synced:   true, // prior sync happened, so this is a real diff
		networks: []config.Network{{Name: "dev-lan", CIDR: "10.0.0.0/24"}},
	}
	cfg := config.Config{Networks: []config.Network{
		{Name: "dev-lan", CIDR: "10.0.1.0/24"}, // changed
		{Name: "new-lan", CIDR: "10.0.2.0/24"}, // added
	}}

	res, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, store, time.Now())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.Commit != "deadbeef" {
		t.Errorf("Commit = %q, want deadbeef", res.Commit)
	}
	// The poller relies on this diff being populated — it's the whole reason
	// SyncOnce exists rather than each caller re-deriving it.
	if len(res.Diff.AddedNetworks) != 1 || len(res.Diff.ChangedNetworks) != 1 {
		t.Errorf("Diff = %+v, want 1 added / 1 changed network", res.Diff)
	}
	if !store.replaced {
		t.Error("SyncOnce did not call Store.Replace")
	}
}

func TestSyncOnceClassifiesErrors(t *testing.T) {
	t.Run("sync failure wraps ErrSync", func(t *testing.T) {
		_, err := SyncOnce(context.Background(), fakeSyncer{err: errors.New("clone failed")}, &fakeStore{}, time.Now())
		if !errors.Is(err, ErrSync) {
			t.Errorf("err = %v, want it to wrap ErrSync", err)
		}
		if errors.Is(err, ErrStore) {
			t.Errorf("err = %v, should not wrap ErrStore", err)
		}
	})

	t.Run("store failure wraps ErrStore", func(t *testing.T) {
		syncer := fakeSyncer{cfg: config.Config{}, sha: "deadbeef"}
		_, err := SyncOnce(context.Background(), syncer, &fakeStore{replaceErr: errors.New("disk full")}, time.Now())
		if !errors.Is(err, ErrStore) {
			t.Errorf("err = %v, want it to wrap ErrStore", err)
		}
		if errors.Is(err, ErrSync) {
			t.Errorf("err = %v, should not wrap ErrSync", err)
		}
	})
}

// concurrencyProbe is a Syncer that records the peak number of Sync calls
// running at once, so a test can assert Service.Sync serializes them.
type concurrencyProbe struct {
	mu      sync.Mutex
	active  int
	maxSeen int
}

func (p *concurrencyProbe) Sync(context.Context) (config.Config, string, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.maxSeen {
		p.maxSeen = p.active
	}
	p.mu.Unlock()

	time.Sleep(2 * time.Millisecond) // widen the window an interleave would show in

	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return config.Config{}, "sha", nil
}

func TestServiceSyncSerializesCalls(t *testing.T) {
	probe := &concurrencyProbe{}
	svc := NewService(probe, nil) // nil store: exercise the lock without persistence

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Sync(context.Background(), time.Now()); err != nil {
				t.Errorf("Sync: %v", err)
			}
		}()
	}
	wg.Wait()

	if probe.maxSeen != 1 {
		t.Errorf("max concurrent syncs = %d, want 1 (Service.Sync should serialize)", probe.maxSeen)
	}
}

func TestSyncOnceNilStoreSkipsReplace(t *testing.T) {
	cfg := config.Config{Networks: []config.Network{{Name: "dev-lan"}}}

	res, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, nil, time.Now())
	if err != nil {
		t.Fatalf("SyncOnce with nil store: %v", err)
	}
	if res.Commit != "deadbeef" || len(res.Config.Networks) != 1 {
		t.Errorf("res = %+v, want commit deadbeef and 1 network", res)
	}
	if !res.Diff.Empty() {
		t.Errorf("Diff = %+v, want empty (no store to diff against)", res.Diff)
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

func TestNetworksInstancesEmptyReturnArray(t *testing.T) {
	// A store that has synced but holds nothing returns nil slices; the API
	// must still serve "[]", not "null", so clients get a stable array shape.
	for _, path := range []string{"/networks", "/instances"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		New(nil, &fakeStore{}).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", path, rec.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
			t.Errorf("GET %s body = %q, want %q", path, got, "[]")
		}
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
