package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// Test fixtures construct netip-typed config values; these helpers keep the
// table fixtures terse.
func prefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr     { return netip.MustParseAddr(s) }
func rng(s string) config.Range {
	var r config.Range
	if err := r.UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return r
}

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
	apps        []config.App
	appsErr     error

	networkByName  map[string]config.Network
	instanceByName map[string]config.Instance
	networkErr     error // if set, Network returns this instead of a networkByName lookup
	instanceErr    error // if set, Instance returns this instead of an instanceByName lookup
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

func (f *fakeStore) Apps(context.Context) ([]config.App, error) {
	return f.apps, f.appsErr
}

func (f *fakeStore) Network(_ context.Context, name string) (config.Network, bool, error) {
	if f.networkErr != nil {
		return config.Network{}, false, f.networkErr
	}
	n, ok := f.networkByName[name]
	return n, ok, nil
}

func (f *fakeStore) Instance(_ context.Context, name string) (config.Instance, bool, error) {
	if f.instanceErr != nil {
		return config.Instance{}, false, f.instanceErr
	}
	i, ok := f.instanceByName[name]
	return i, ok, nil
}

// fakeTunnelSource is a no-op TunnelSource: enough for handlers/SyncOnce to
// treat WireGuard as "configured" without a real in-process tunnel.
type fakeTunnelSource struct {
	pub      wireguard.PublicKey
	endpoint string

	upsertErr  error
	upsertCall []struct {
		pub      wireguard.PublicKey
		tunnelIP netip.Addr
	}
}

func (f *fakeTunnelSource) PublicKey() wireguard.PublicKey { return f.pub }
func (f *fakeTunnelSource) Endpoint() string               { return f.endpoint }

func (f *fakeTunnelSource) UpsertPeer(pub wireguard.PublicKey, tunnelIP netip.Addr) error {
	f.upsertCall = append(f.upsertCall, struct {
		pub      wireguard.PublicKey
		tunnelIP netip.Addr
	}{pub, tunnelIP})
	return f.upsertErr
}

func (f *fakeTunnelSource) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("fakeTunnelSource: DialContext not implemented")
}

func (f *fakeTunnelSource) Close() error { return nil }

// fakeCredentialStore is an in-memory nodeprovision.CredentialStore.
type fakeCredentialStore struct {
	mu      sync.Mutex
	creds   map[string]nodeprovision.Credential
	readErr error // if set, InstanceCredential returns this instead of a lookup
}

func (f *fakeCredentialStore) InstanceCredential(_ context.Context, name string) (nodeprovision.Credential, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nodeprovision.Credential{}, false, f.readErr
	}
	c, ok := f.creds[name]
	return c, ok, nil
}

// SetInstanceCredential mirrors internal/store's real ON CONFLICT DO
// NOTHING semantics (insert once per name) — see
// internal/nodeprovision's identical fake for why.
func (f *fakeCredentialStore) SetInstanceCredential(_ context.Context, name string, cred nodeprovision.Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creds == nil {
		f.creds = make(map[string]nodeprovision.Credential)
	}
	if _, exists := f.creds[name]; exists {
		return nil
	}
	f.creds[name] = cred
	return nil
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	New(nil, nil, nil, nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSyncNotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(nil, nil, nil, nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /sync with nil syncer = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestSyncSuccess(t *testing.T) {
	cfg := config.Config{
		Networks:  []config.Network{{Name: "dev-lan", CIDR: prefix("10.0.0.0/24")}},
		Instances: []config.Instance{{Name: "devnode0", Network: "dev-lan", StaticIP: addr("10.0.0.5")}},
	}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}
	store := &fakeStore{}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store, nil, nil, nil, nil).ServeHTTP(rec, req)

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
		networks:  []config.Network{{Name: "dev-lan", CIDR: prefix("10.0.0.0/24")}},
		instances: []config.Instance{{Name: "devnode0"}},
	}
	cfg := config.Config{
		Networks: []config.Network{
			{Name: "dev-lan", CIDR: prefix("10.0.1.0/24")}, // changed
			{Name: "new-lan", CIDR: prefix("10.0.2.0/24")}, // added
		},
		// devnode0 removed, no instances in the new sync
	}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store, nil, nil, nil, nil).ServeHTTP(rec, req)

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
	cfg := config.Config{Networks: []config.Network{{Name: "dev-lan", CIDR: prefix("10.0.0.0/24")}}}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store, nil, nil, nil, nil).ServeHTTP(rec, req)

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

	New(syncer, nil, nil, nil, nil, nil).ServeHTTP(rec, req)

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

	New(syncer, store, nil, nil, nil, nil).ServeHTTP(rec, req)

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
		networks: []config.Network{{Name: "dev-lan", CIDR: prefix("10.0.0.0/24")}},
	}
	cfg := config.Config{Networks: []config.Network{
		{Name: "dev-lan", CIDR: prefix("10.0.1.0/24")}, // changed
		{Name: "new-lan", CIDR: prefix("10.0.2.0/24")}, // added
	}}

	res, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, store, nil, nil, time.Now())
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
		_, err := SyncOnce(context.Background(), fakeSyncer{err: errors.New("clone failed")}, &fakeStore{}, nil, nil, time.Now())
		if !errors.Is(err, ErrSync) {
			t.Errorf("err = %v, want it to wrap ErrSync", err)
		}
		if errors.Is(err, ErrStore) {
			t.Errorf("err = %v, should not wrap ErrStore", err)
		}
	})

	t.Run("store failure wraps ErrStore", func(t *testing.T) {
		syncer := fakeSyncer{cfg: config.Config{}, sha: "deadbeef"}
		_, err := SyncOnce(context.Background(), syncer, &fakeStore{replaceErr: errors.New("disk full")}, nil, nil, time.Now())
		if !errors.Is(err, ErrStore) {
			t.Errorf("err = %v, want it to wrap ErrStore", err)
		}
		if errors.Is(err, ErrSync) {
			t.Errorf("err = %v, should not wrap ErrSync", err)
		}
	})
}

// TestSyncOnceAutoAssignsStaticIP covers the auto-assignment path end to
// end through SyncOnce: an instance with no static_ip on a network with a
// dhcp_excluded_range gets one drawn from that range, and the assignment is
// what actually gets persisted via Store.Replace.
func TestSyncOnceAutoAssignsStaticIP(t *testing.T) {
	cfg := config.Config{
		Networks:  []config.Network{{Name: "lan", CIDR: prefix("192.168.1.0/24"), DHCPExcludedRange: rng("192.168.1.200-192.168.1.203")}},
		Instances: []config.Instance{{Name: "node-a", Network: "lan"}},
	}
	store := &fakeStore{}

	res, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, store, nil, nil, time.Now())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if got, want := res.Config.Instances[0].StaticIP.String(), "192.168.1.200"; got != want {
		t.Errorf("StaticIP = %q, want %q", got, want)
	}
	if got, want := store.replaceCfg.Instances[0].StaticIP.String(), "192.168.1.200"; got != want {
		t.Errorf("Replace persisted StaticIP = %q, want %q", got, want)
	}
}

// TestSyncOnceAutoAssignmentIsStableAcrossResyncs guards the reuse-then-fill
// requirement: an instance that already has an auto-assigned address must
// keep it on the next sync rather than drawing a different one, since the
// instance's YAML still omits static_ip on every poll.
func TestSyncOnceAutoAssignmentIsStableAcrossResyncs(t *testing.T) {
	cfg := config.Config{
		Networks:  []config.Network{{Name: "lan", CIDR: prefix("192.168.1.0/24"), DHCPExcludedRange: rng("192.168.1.200-192.168.1.203")}},
		Instances: []config.Instance{{Name: "node-a", Network: "lan"}},
	}
	store := &fakeStore{}

	first, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "sha1"}, store, nil, nil, time.Now())
	if err != nil {
		t.Fatalf("first SyncOnce: %v", err)
	}
	firstIP := first.Config.Instances[0].StaticIP

	// Simulate the next poll: the store now reflects the prior assignment,
	// and the freshly re-parsed YAML still has no explicit static_ip.
	store.synced = true
	store.networks = cfg.Networks
	store.instances = first.Config.Instances

	second, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "sha2"}, store, nil, nil, time.Now())
	if err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	if got := second.Config.Instances[0].StaticIP; got != firstIP {
		t.Errorf("StaticIP churned across resync: first %q, second %q", firstIP, got)
	}
}

// TestSyncOnceExplicitStaticIPConflictsWithPriorAssignedIP covers the
// store-backed half of the prior-IP-reservation policy (docs/Ipam.md):
// an explicit static_ip in the freshly synced config that collides with a
// *different* instance's address already persisted from a prior sync must
// hard-fail, not silently relocate the prior holder. TestSyncOnceIPAMFailures
// below exercises the same policy with an empty store; this one exercises it
// through the actual store-read path SyncOnce uses for stability.
func TestSyncOnceExplicitStaticIPConflictsWithPriorAssignedIP(t *testing.T) {
	lan := config.Network{Name: "lan", CIDR: prefix("192.168.1.0/24"), DHCPExcludedRange: rng("192.168.1.200-192.168.1.203")}
	store := &fakeStore{
		synced:    true,
		networks:  []config.Network{lan},
		instances: []config.Instance{{Name: "node-a", Network: "lan", StaticIP: addr("192.168.1.200")}},
	}
	cfg := config.Config{
		Networks: []config.Network{lan},
		Instances: []config.Instance{
			{Name: "node-a", Network: "lan"},                                  // omits static_ip; would normally reuse .200
			{Name: "node-b", Network: "lan", StaticIP: addr("192.168.1.200")}, // reserved for node-a
		},
	}

	_, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, store, nil, nil, time.Now())
	if !errors.Is(err, ErrIPAM) {
		t.Fatalf("err = %v, want it to wrap ErrIPAM", err)
	}
	if store.replaced {
		t.Error("SyncOnce called Store.Replace despite a prior-IP conflict")
	}
}

// TestSyncOnceIPAMFailures covers the data-integrity cases that must hard
// fail a sync (no Store.Replace) rather than silently persisting bad state.
func TestSyncOnceIPAMFailures(t *testing.T) {
	lan := config.Network{Name: "lan", CIDR: prefix("192.168.1.0/24"), DHCPExcludedRange: rng("192.168.1.200-192.168.1.200")}

	// These are the assignment-time failures only ipam can express. An
	// out-of-range *explicit* static_ip is no longer here: it's a semantic
	// problem config.Validate catches first (see TestSyncOnceValidationFailure),
	// so it surfaces as ErrValidate, not ErrIPAM.
	cases := []struct {
		name      string
		instances []config.Instance
	}{
		{
			name: "duplicate explicit static_ip on the same network",
			instances: []config.Instance{
				{Name: "node-a", Network: "lan", StaticIP: addr("192.168.1.200")},
				{Name: "node-b", Network: "lan", StaticIP: addr("192.168.1.200")},
			},
		},
		{
			name: "pool exhausted",
			instances: []config.Instance{
				{Name: "node-a", Network: "lan"},
				{Name: "node-b", Network: "lan"}, // only one address (.200) in the pool
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Networks: []config.Network{lan}, Instances: tc.instances}
			store := &fakeStore{}

			_, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, store, nil, nil, time.Now())
			if !errors.Is(err, ErrIPAM) {
				t.Fatalf("err = %v, want it to wrap ErrIPAM", err)
			}
			if store.replaced {
				t.Error("SyncOnce called Store.Replace despite an IPAM failure")
			}
		})
	}
}

// TestSyncOnceValidationFailure covers the semantic-validation stage: a config
// that parses fine but is semantically wrong (an explicit static_ip outside its
// network's CIDR) must fail with ErrValidate before IPAM runs, map to HTTP 502,
// and never reach Store.Replace.
func TestSyncOnceValidationFailure(t *testing.T) {
	cfg := config.Config{
		Networks:  []config.Network{{Name: "lan", CIDR: prefix("192.168.1.0/24")}},
		Instances: []config.Instance{{Name: "node-a", Network: "lan", StaticIP: addr("10.0.0.5")}},
	}

	t.Run("SyncOnce wraps ErrValidate and skips Replace", func(t *testing.T) {
		store := &fakeStore{}
		_, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, store, nil, nil, time.Now())
		if !errors.Is(err, ErrValidate) {
			t.Fatalf("err = %v, want it to wrap ErrValidate", err)
		}
		if errors.Is(err, ErrIPAM) {
			t.Errorf("err = %v, should not wrap ErrIPAM (validation runs first)", err)
		}
		if store.replaced {
			t.Error("SyncOnce called Store.Replace despite a validation failure")
		}
	})

	t.Run("POST /sync maps ErrValidate to 502", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/sync", nil)
		rec := httptest.NewRecorder()
		New(fakeSyncer{cfg: cfg, sha: "deadbeef"}, &fakeStore{}, nil, nil, nil, nil).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("POST /sync with invalid config = %d, want %d", rec.Code, http.StatusBadGateway)
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
	svc := NewService(probe, nil, nil, nil, nil, nil) // nil store: exercise the lock without persistence

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

	res, err := SyncOnce(context.Background(), fakeSyncer{cfg: cfg, sha: "deadbeef"}, nil, nil, nil, time.Now())
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

// TestReconcileTunnelPeersUpsertsEachInstance is a focused test on
// reconcileTunnelPeers directly — the mechanism that makes "no live
// enrollment step" true for WireGuard: every synced instance must end up
// registered as a trusted peer on the live tunnel with its minted
// credential's public key and its assigned tunnel IP. Previously only
// exercised incidentally by SyncOnce-level tests, which never asserted on
// fakeTunnelSource's upsertCall/upsertErr fields despite those fields
// existing specifically for this.
func TestReconcileTunnelPeersUpsertsEachInstance(t *testing.T) {
	tunnels := &fakeTunnelSource{}
	creds := &fakeCredentialStore{}
	instances := []config.Instance{
		{Name: "node-a", TunnelIP: addr("10.100.0.2")},
		{Name: "node-b", TunnelIP: addr("10.100.0.3")},
	}

	reconcileTunnelPeers(context.Background(), tunnels, creds, instances)

	if len(tunnels.upsertCall) != 2 {
		t.Fatalf("len(upsertCall) = %d, want 2", len(tunnels.upsertCall))
	}
	for i, inst := range instances {
		cred, ok, err := creds.InstanceCredential(context.Background(), inst.Name)
		if err != nil || !ok {
			t.Fatalf("credential for %q = ok:%v err:%v, want minted", inst.Name, ok, err)
		}
		wantPub := wireguard.PublicKeyOf(cred.WireGuardPrivateKey)
		call := tunnels.upsertCall[i]
		if call.pub != wantPub {
			t.Errorf("upsertCall[%d].pub = %s, want %s (the minted credential's public key)", i, call.pub, wantPub)
		}
		if call.tunnelIP != inst.TunnelIP {
			t.Errorf("upsertCall[%d].tunnelIP = %s, want %s", i, call.tunnelIP, inst.TunnelIP)
		}
	}
}

// TestReconcileTunnelPeersContinuesPastUpsertError proves the
// logged-and-continue posture the doc comment on SyncOnce/
// reconcileTunnelPeers describes: one instance's UpsertPeer failure must
// not stop later instances in the same reconcile pass from being
// registered.
func TestReconcileTunnelPeersContinuesPastUpsertError(t *testing.T) {
	tunnels := &fakeTunnelSource{upsertErr: errors.New("device busy")}
	creds := &fakeCredentialStore{}
	instances := []config.Instance{
		{Name: "node-a", TunnelIP: addr("10.100.0.2")},
		{Name: "node-b", TunnelIP: addr("10.100.0.3")},
	}

	reconcileTunnelPeers(context.Background(), tunnels, creds, instances)

	if len(tunnels.upsertCall) != 2 {
		t.Fatalf("len(upsertCall) = %d, want 2 (an UpsertPeer error must not stop the reconcile loop)", len(tunnels.upsertCall))
	}
}

// TestReconcileTunnelPeersContinuesPastCredentialError mirrors the above
// for EnsureCredential failing on one instance.
func TestReconcileTunnelPeersContinuesPastCredentialError(t *testing.T) {
	tunnels := &fakeTunnelSource{}
	creds := &credentialStoreFailingFor{name: "node-a", err: errors.New("disk full")}
	instances := []config.Instance{
		{Name: "node-a", TunnelIP: addr("10.100.0.2")},
		{Name: "node-b", TunnelIP: addr("10.100.0.3")},
	}

	reconcileTunnelPeers(context.Background(), tunnels, creds, instances)

	// node-a's EnsureCredential fails, so it must produce no UpsertPeer
	// call — but node-b, later in the same slice, must still be reached
	// and registered rather than the loop aborting after node-a's error.
	if len(tunnels.upsertCall) != 1 {
		t.Fatalf("len(upsertCall) = %d, want 1 (node-b, reached after node-a's credential error)", len(tunnels.upsertCall))
	}
	if tunnels.upsertCall[0].tunnelIP != addr("10.100.0.3") {
		t.Errorf("upsertCall[0].tunnelIP = %s, want node-b's 10.100.0.3", tunnels.upsertCall[0].tunnelIP)
	}
}

// credentialStoreFailingFor is a nodeprovision.CredentialStore that fails
// InstanceCredential for exactly one instance name and behaves like a
// normal in-memory store for every other name — used to prove
// reconcileTunnelPeers's loop keeps going past one instance's error
// instead of aborting, which a store that fails unconditionally for every
// name can't distinguish from "aborted after the first failure".
type credentialStoreFailingFor struct {
	name string
	err  error
	fakeCredentialStore
}

func (c *credentialStoreFailingFor) InstanceCredential(ctx context.Context, name string) (nodeprovision.Credential, bool, error) {
	if name == c.name {
		return nodeprovision.Credential{}, false, c.err
	}
	return c.fakeCredentialStore.InstanceCredential(ctx, name)
}

func TestStatusNotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	New(nil, nil, nil, nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /status with nil store = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestStatusNeverSynced(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	New(nil, &fakeStore{synced: false}, nil, nil, nil, nil).ServeHTTP(rec, req)

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

	New(nil, store, nil, nil, nil, nil).ServeHTTP(rec, req)

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
	New(nil, store, nil, nil, nil, nil).ServeHTTP(rec, req)
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
	New(nil, store, nil, nil, nil, nil).ServeHTTP(rec, req)
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

		New(nil, &fakeStore{}, nil, nil, nil, nil).ServeHTTP(rec, req)

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

		New(nil, nil, nil, nil, nil, nil).ServeHTTP(rec, req)

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

		New(nil, nil, nil, nil, nil, nil).ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// Apps must reach the diff the same way networks/instances do: the stored
// snapshot is the baseline, and an agent image bump shows up as a change.
func TestSyncWithAppDiff(t *testing.T) {
	agentV1 := config.App{
		Name: "agent", Type: "agent", Replicas: config.Replicas{PerNode: true},
		Image: config.ImageRef{Alias: "agent:v1"},
	}
	agentV2 := agentV1
	agentV2.Image = config.ImageRef{Alias: "agent:v2"}

	store := &fakeStore{
		synced: true, // a prior sync happened, so this is not a "first sync"
		apps: []config.App{
			agentV1,
			{Name: "old-app", Type: "some-renderer", Replicas: config.Replicas{Count: 1}, Image: config.ImageRef{Alias: "o:v1"}},
		},
	}
	cfg := config.Config{Apps: []config.App{
		agentV2, // changed
		{Name: "new-app", Type: "some-renderer", Replicas: config.Replicas{Count: 1}, Image: config.ImageRef{Alias: "n:v1"}}, // added
		// old-app removed
	}}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store, nil, nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sync = %d, want %d", rec.Code, http.StatusOK)
	}

	var got syncResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := diffCounts{AppsAdded: 1, AppsChanged: 1, AppsRemoved: 1}
	if got.Diff != want {
		t.Errorf("Diff = %+v, want %+v", got.Diff, want)
	}
	if !reflect.DeepEqual(store.replaceCfg.Apps, cfg.Apps) {
		t.Errorf("Replace got Apps = %+v, want %+v", store.replaceCfg.Apps, cfg.Apps)
	}
}

// Mirrors TestSyncDiffReadFailure for the new reader: a failure reading prior
// Apps is treated as no baseline, never failing the sync.
func TestSyncAppsReadFailure(t *testing.T) {
	store := &fakeStore{synced: true, appsErr: errors.New("disk full")}
	cfg := config.Config{Apps: []config.App{
		{Name: "agent", Type: "agent", Replicas: config.Replicas{PerNode: true}, Image: config.ImageRef{Alias: "agent:v1"}},
	}}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	New(syncer, store, nil, nil, nil, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sync with failing apps read = %d, want %d", rec.Code, http.StatusOK)
	}
	if !store.replaced {
		t.Fatalf("POST /sync did not call Store.Replace despite a failed apps read")
	}
}

// An invalid App fails the whole sync as upstream's fault, exactly as an
// invalid Network does — this is the end-to-end proof that Apps thread through
// config.Validate inside SyncOnce.
func TestSyncOnceRejectsInvalidApp(t *testing.T) {
	cfg := config.Config{Apps: []config.App{
		// replicas omitted: required, so this is a hard validation failure.
		{Name: "agent", Type: "agent", Image: config.ImageRef{Alias: "agent:v1"}},
	}}
	syncer := fakeSyncer{cfg: cfg, sha: "deadbeef"}
	store := &fakeStore{}

	_, err := SyncOnce(context.Background(), syncer, store, nil, nil, time.Now())
	if !errors.Is(err, ErrValidate) {
		t.Fatalf("SyncOnce error = %v, want ErrValidate", err)
	}
	if store.replaced {
		t.Errorf("SyncOnce called Replace despite a validation failure")
	}
}
