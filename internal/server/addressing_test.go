package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/addressing"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/store"
)

// These tests cover the sync's resolve step (docs/Decisions.md §28, #199):
// network addressing from git or from the web app's state, one source per
// network, joined before validation, IPAM and the seed route.

// fakeAddressing is an AddressingSource whose networks a test can change
// between syncs.
type fakeAddressing struct {
	nets  []config.Network
	err   error
	calls int
}

func (f *fakeAddressing) Networks(context.Context) ([]config.Network, error) {
	f.calls++
	return f.nets, f.err
}

// labAddressing is lab's full addressing, in git or in state.
func labAddressing() config.Network {
	return config.Network{
		Name:              "lab",
		CIDR:              prefix("10.0.2.0/24"),
		Gateway:           addr("10.0.2.1"),
		DHCPExcludedRange: rng("10.0.2.200-10.0.2.250"),
		DNS:               []netip.Addr{addr("10.0.2.1")},
	}
}

// mixedFleet is staticIPLessFleet (git-addressed dev-lan, devnode1) plus a
// network lab, git-addressed or name-only, and an instance labnode on it.
func mixedFleet(labInGit bool) config.Config {
	cfg := staticIPLessFleet()
	lab := config.Network{Name: "lab"}
	if labInGit {
		lab = labAddressing()
	}
	cfg.Networks = append(cfg.Networks, lab)
	cfg.Instances = append(cfg.Instances, config.Instance{
		Name: "labnode", MAC: "aa:bb:cc:dd:ee:02", Network: "lab",
		Disk: "single", NIC: "single", Applications: []string{"incus"},
	})
	return cfg
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSyncOnceRejectsGitAddressingByDefault(t *testing.T) {
	st := &fakeStore{}
	_, err := SyncOnce(context.Background(), fakeSyncer{cfg: staticIPLessFleet(), sha: "c1"}, st, nil, nil, Addressing{}, time.Now())
	if !errors.Is(err, ErrResolve) || !strings.Contains(err.Error(), `network "dev-lan" is addressed in git, but CONFIG_REPO_PRIVATE is not "true"`) {
		t.Fatalf("SyncOnce = %v, want ErrResolve naming dev-lan and CONFIG_REPO_PRIVATE", err)
	}
	if st.replaced {
		t.Error("a rejected sync replaced the stored snapshot")
	}
}

func TestSyncOnceAddressingSourceError(t *testing.T) {
	src := &fakeAddressing{err: errors.New("open addressing file: no such file")}
	_, err := SyncOnce(context.Background(), fakeSyncer{cfg: mixedFleet(false), sha: "c1"}, &fakeStore{}, nil, nil, Addressing{RepoPrivate: true, State: src}, time.Now())
	if !errors.Is(err, ErrAddressing) {
		t.Fatalf("SyncOnce = %v, want ErrAddressing", err)
	}
}

// TestSyncAddressingSourceErrorIs500: an unreadable addressing file is this
// deployment's fault, not the git remote's, so POST /sync answers 500 naming
// the file rather than 502.
func TestSyncAddressingSourceErrorIs500(t *testing.T) {
	svc := NewService(fakeSyncer{cfg: mixedFleet(false), sha: "c1"}, &fakeStore{}, nil, nil, nil, nil)
	svc.SetAddressing(Addressing{RepoPrivate: true, State: &fakeAddressing{err: errors.New("open addressing file: no such file")}})
	rec := httptest.NewRecorder()
	NewFromService(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sync", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "NETWORK_ADDRESSING_PATH") {
		t.Errorf("POST /sync = %d %q, want 500 naming NETWORK_ADDRESSING_PATH", rec.Code, rec.Body.String())
	}
}

func TestSyncOnceRejectsUnresolvableAddressing(t *testing.T) {
	tests := []struct {
		name  string
		cfg   config.Config
		state []config.Network
		want  string
	}{
		{"no state entry", mixedFleet(false), nil, `network "lab" has no addressing`},
		{"addressed in both", mixedFleet(true), []config.Network{labAddressing()}, `network "lab" is addressed both in git and in the addressing file`},
		{"unknown state entry", staticIPLessFleet(), []config.Network{labAddressing()}, `network "lab" is not declared in git`},
		{"state entry with no cidr", mixedFleet(false), []config.Network{{Name: "lab", Gateway: addr("10.0.2.1")}}, `network "lab": an addressing entry must set a cidr`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeAddressing{nets: tc.state}
			_, err := SyncOnce(context.Background(), fakeSyncer{cfg: tc.cfg, sha: "c1"}, &fakeStore{}, nil, nil, Addressing{RepoPrivate: true, State: src}, time.Now())
			if !errors.Is(err, ErrResolve) || errors.Is(err, ErrValidate) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SyncOnce = %v, want ErrResolve, not ErrValidate, containing %q", err, tc.want)
			}
		})
	}
}

// TestSyncAddressingRejectionStatus: POST /sync answers a resolution that
// doesn't fit this web app's addressing with 409, naming the settings, not
// with the 502 that blames the git remote. Addressing git gets wrong on its
// own is still a 502: the commit needs fixing whatever the settings.
func TestSyncAddressingRejectionStatus(t *testing.T) {
	partial := mixedFleet(false)
	partial.Networks[1].Gateway = addr("10.0.2.1")
	for _, tc := range []struct {
		name   string
		cfg    config.Config
		addr   Addressing
		code   int
		inBody string
	}{
		{"git addressing in a public repo", staticIPLessFleet(), Addressing{}, http.StatusConflict, "CONFIG_REPO_PRIVATE"},
		{"an entry for an undeclared network", staticIPLessFleet(), Addressing{RepoPrivate: true, State: &fakeAddressing{nets: []config.Network{labAddressing()}}}, http.StatusConflict, "NETWORK_ADDRESSING_PATH"},
		{"partial addressing in git", partial, Addressing{RepoPrivate: true, State: &fakeAddressing{nets: []config.Network{labAddressing()}}}, http.StatusBadGateway, "sync failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(fakeSyncer{cfg: tc.cfg, sha: "c1"}, &fakeStore{}, nil, nil, nil, nil)
			svc.SetAddressing(tc.addr)
			rec := httptest.NewRecorder()
			NewFromService(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sync", nil))
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.inBody) {
				t.Errorf("POST /sync = %d %q, want %d containing %q", rec.Code, rec.Body.String(), tc.code, tc.inBody)
			}
		})
	}
}

// TestSyncOnceMixedFleet: a fleet with one network from each source syncs,
// IPAM assigns on both, and both instances' seeds render with their own
// network's addressing.
func TestSyncOnceMixedFleet(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	tunnels := &fakeTunnelSource{}
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}
	src := &fakeAddressing{nets: []config.Network{labAddressing()}}

	res, err := SyncOnce(ctx, fakeSyncer{cfg: mixedFleet(false), sha: "c1"}, st, tunnels, st, Addressing{RepoPrivate: true, State: src}, time.Now())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if want := []string{`network "lab": addressing loaded from the web app's state: cidr 10.0.2.0/24, gateway 10.0.2.1, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1]`}; !reflect.DeepEqual(messages(res.Warnings), want) {
		t.Errorf("Warnings = %q, want %q", res.Warnings, want)
	}
	if want := []string{"lab"}; !reflect.DeepEqual(res.StateAddressed, want) {
		t.Errorf("StateAddressed = %q, want %q", res.StateAddressed, want)
	}

	for name, want := range map[string][]string{
		"devnode1": {"10.0.0.200/24", "10.0.0.1"},
		"labnode":  {"10.0.2.200/24", "10.0.2.1"},
	} {
		resp := fetchInstanceSeed(t, st, certs, tunnels, st, name)
		for _, w := range want {
			if !strings.Contains(resp.NetworkYAML, w) {
				t.Errorf("%s network_yaml lacks %s:\n%s", name, w, resp.NetworkYAML)
			}
		}
	}
}

// TestSeedIdenticalAcrossAddressingModes: the same instance renders a
// byte-identical seed whether its network's (identical) addressing is in git
// or in state, and moving it between the two reports no change.
func TestSeedIdenticalAcrossAddressingModes(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	tunnels := &fakeTunnelSource{}
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	if _, err := SyncOnce(ctx, fakeSyncer{cfg: mixedFleet(true), sha: "c1"}, st, tunnels, st, privateRepo, time.Now()); err != nil {
		t.Fatalf("SyncOnce (lab in git): %v", err)
	}
	fromGit := fetchInstanceSeed(t, st, certs, tunnels, st, "labnode")

	src := &fakeAddressing{nets: []config.Network{labAddressing()}}
	res, err := SyncOnce(ctx, fakeSyncer{cfg: mixedFleet(false), sha: "c2"}, st, tunnels, st, Addressing{RepoPrivate: true, State: src}, time.Now())
	if err != nil {
		t.Fatalf("SyncOnce (lab in state): %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none: the addressing didn't change", res.Warnings)
	}
	fromState := fetchInstanceSeed(t, st, certs, tunnels, st, "labnode")

	a, _ := json.Marshal(fromGit)
	b, _ := json.Marshal(fromState)
	if string(a) != string(b) {
		t.Errorf("seeds differ:\ngit:   %s\nstate: %s", a, b)
	}
}

// TestSyncRereadsAddressingAtSameCommit: the addressing source is read on
// every sync, so an edit to it lands, and is reported in the sync's and
// GET /status's warnings, though the commit hasn't moved.
func TestSyncRereadsAddressingAtSameCommit(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	src := &fakeAddressing{nets: []config.Network{labAddressing()}}
	svc := NewService(fakeSyncer{cfg: mixedFleet(false), sha: "c1"}, st, nil, nil, nil, nil)
	svc.SetAddressing(Addressing{RepoPrivate: true, State: src})

	if _, err := svc.Sync(ctx, time.Now()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	moved := labAddressing()
	moved.Gateway = addr("10.0.2.254")
	src.nets = []config.Network{moved}
	res, err := svc.Sync(ctx, time.Now())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if src.calls != 2 {
		t.Errorf("addressing source read %d times over two syncs, want 2", src.calls)
	}
	n, _, err := st.Network(ctx, "lab")
	if err != nil || n.Gateway != moved.Gateway {
		t.Errorf("stored lab gateway = %v (err %v), want %v", n.Gateway, err, moved.Gateway)
	}
	want := `network "lab": addressing from the web app's state changed: cidr 10.0.2.0/24, gateway 10.0.2.254, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1], was cidr 10.0.2.0/24, gateway 10.0.2.1, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1]`
	if !reflect.DeepEqual(messages(res.Warnings), []string{want}) {
		t.Errorf("Warnings = %q, want [%q]", res.Warnings, want)
	}

	// A third sync finds the addressing unchanged, so it reports nothing,
	// but /status keeps the change until the addressing changes again.
	if res, err := svc.Sync(ctx, time.Now()); err != nil || len(res.Warnings) != 0 {
		t.Fatalf("third Sync = %q, %v; want no warnings", res.Warnings, err)
	}
	if got := statusWarnings(t, svc); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("/status sync_warnings = %q after a sync with none, want still [%q]", got, want)
	}

	// Once lab moves into git, its addressing no longer comes from state,
	// so /status drops the warning though this sync reports none.
	svc.syncer = fakeSyncer{cfg: mixedFleet(true), sha: "c2"}
	src.nets = nil
	if res, err := svc.Sync(ctx, time.Now()); err != nil || len(res.Warnings) != 0 {
		t.Fatalf("Sync with lab in git = %q, %v; want no warnings", res.Warnings, err)
	}
	if got := statusWarnings(t, svc); len(got) != 0 {
		t.Errorf("/status sync_warnings = %q after lab moved into git, want none", got)
	}
}

func statusWarnings(t *testing.T, svc *Service) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	NewFromService(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var status statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	return status.SyncWarnings
}

// TestCarryWarnings: a network's warning stays until a newer one replaces
// it or the network is no longer state-addressed.
func TestCarryWarnings(t *testing.T) {
	w := func(network, msg string) addressing.Change { return addressing.Change{Network: network, Message: msg} }
	prev := []addressing.Change{w("lab", "lab loaded"), w("dmz", "dmz loaded"), w("old", "old loaded")}
	res := SyncResult{StateAddressed: []string{"lab", "dmz", "new"}, Warnings: []addressing.Change{w("dmz", "dmz changed"), w("new", "new loaded")}}
	got := messages(carryWarnings(prev, res))
	if want := []string{"lab loaded", "dmz changed", "new loaded"}; !reflect.DeepEqual(got, want) {
		t.Errorf("carryWarnings = %q, want %q", got, want)
	}
}

// TestSyncOnceUnreadablePriorReportsNoChanges: with the prior snapshot
// unreadable, the sync still lands, but reports no state-addressed network
// as loaded: it has nothing to compare against, and such lines would stay
// on /status.
func TestSyncOnceUnreadablePriorReportsNoChanges(t *testing.T) {
	st := &fakeStore{synced: true, commit: "c0", networksErr: errors.New("db locked")}
	src := &fakeAddressing{nets: []config.Network{labAddressing()}}
	res, err := SyncOnce(context.Background(), fakeSyncer{cfg: mixedFleet(false), sha: "c1"}, st, nil, nil, Addressing{RepoPrivate: true, State: src}, time.Now())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if !st.replaced {
		t.Error("the sync didn't land")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none with no prior snapshot read", res.Warnings)
	}
}
