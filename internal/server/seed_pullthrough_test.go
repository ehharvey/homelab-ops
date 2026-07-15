package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/store"
)

// These tests prove issue #38's pull-through: an Instance synced *without* a
// static_ip still reaches seed.Render with a concrete, IPAM-assigned address
// (and default route) by the time network.yaml is rendered. Unlike the
// seed-route tests in instances_test.go, which pre-seed StaticIP into a
// fakeStore, these drive the *real* internal/ipam (via SyncOnce) and the *real*
// internal/store (in-memory) so the assignment logic is actually exercised —
// a fakeStore with StaticIP already filled in would pass without ever running
// #35's code (see the analysis on #38). No import cycle: internal/store imports
// only internal/config, and internal/server does not import internal/store.

// staticIPLessFleet is a minimal valid fleet whose sole instance omits
// static_ip, forcing IPAM to auto-assign from dev-lan's dhcp_excluded_range
// (the static pool). The network carries a gateway so seed.Render can emit the
// default route.
func staticIPLessFleet() config.Config {
	return config.Config{
		Networks: []config.Network{{
			Name:              "dev-lan",
			CIDR:              prefix("10.0.0.0/24"),
			Gateway:           addr("10.0.0.1"),
			DHCPExcludedRange: rng("10.0.0.200-10.0.0.250"),
		}},
		Instances: []config.Instance{{
			Name:    "devnode1",
			MAC:     "aa:bb:cc:dd:ee:01",
			Network: "dev-lan",
			// StaticIP deliberately omitted — IPAM must fill it in at sync time.
			Disk:         "single",
			NIC:          "single",
			Applications: []string{"incus"},
		}},
	}
}

// fetchInstanceSeed drives the real seed handler and returns the decoded
// response, failing the test on any non-200.
func fetchInstanceSeed(t *testing.T, st Store, certs CertSource, tunnels TunnelSource, creds nodeprovision.CredentialStore, name string) instanceSeedResponse {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/instances/"+name+"/seed", nil)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()

	handleInstanceSeed(st, certs, tunnels, creds)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /instances/%s/seed = %d, want %d (body %q)", name, rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp instanceSeedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode seed response: %v", err)
	}
	return resp
}

func TestSeedPullThroughAssignsIPForStaticIPLessInstance(t *testing.T) {
	ctx := context.Background()
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tunnels := &fakeTunnelSource{}

	// Real config.Validate + real ipam.Assign (auto-assign) + real store.Replace.
	if _, err := SyncOnce(ctx, fakeSyncer{cfg: staticIPLessFleet(), sha: "commit1"}, st, tunnels, st, time.Now()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	resp := fetchInstanceSeed(t, st, certs, tunnels, st, "devnode1")

	// The assignment must have landed in the persisted snapshot, in-range.
	inst, ok, err := st.Instance(ctx, "devnode1")
	if err != nil || !ok {
		t.Fatalf("store.Instance(devnode1) = ok:%v err:%v, want a persisted instance", ok, err)
	}
	if !inst.StaticIP.IsValid() {
		t.Fatalf("devnode1 has no static_ip after sync; IPAM auto-assignment did not run")
	}
	lo, hi := addr("10.0.0.200"), addr("10.0.0.250")
	if inst.StaticIP.Compare(lo) < 0 || inst.StaticIP.Compare(hi) > 0 {
		t.Fatalf("auto-assigned static_ip %s is outside the pool %s-%s", inst.StaticIP, lo, hi)
	}

	// Consumer-side: the rendered network.yaml carries that address and a
	// default route via the gateway.
	wantAddr := inst.StaticIP.String() + "/24"
	if !strings.Contains(resp.NetworkYAML, wantAddr) {
		t.Errorf("network_yaml missing assigned address %q:\n%s", wantAddr, resp.NetworkYAML)
	}
	if !strings.Contains(resp.NetworkYAML, "0.0.0.0/0") {
		t.Errorf("network_yaml missing default route (0.0.0.0/0):\n%s", resp.NetworkYAML)
	}
	if !strings.Contains(resp.NetworkYAML, "10.0.0.1") {
		t.Errorf("network_yaml missing gateway 10.0.0.1 as the route via:\n%s", resp.NetworkYAML)
	}
}

func TestSeedPullThroughIPStableAcrossResync(t *testing.T) {
	ctx := context.Background()
	cfg := staticIPLessFleet()
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tunnels := &fakeTunnelSource{}

	if _, err := SyncOnce(ctx, fakeSyncer{cfg: cfg, sha: "commit1"}, st, tunnels, st, time.Now()); err != nil {
		t.Fatalf("first SyncOnce: %v", err)
	}
	first := fetchInstanceSeed(t, st, certs, tunnels, st, "devnode1").NetworkYAML

	// Re-syncing the identical fleet must reuse the persisted prior address
	// (SyncOnce feeds the prior snapshot into ipam.Assign), not hand out a new
	// one — otherwise network.yaml would silently churn its IP every poll.
	if _, err := SyncOnce(ctx, fakeSyncer{cfg: cfg, sha: "commit2"}, st, tunnels, st, time.Now()); err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	second := fetchInstanceSeed(t, st, certs, tunnels, st, "devnode1").NetworkYAML

	if first != second {
		t.Errorf("network.yaml changed across re-sync:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
