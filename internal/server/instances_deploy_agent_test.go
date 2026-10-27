package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision/nodeprovisiontest"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

func sampleAgentApp() config.App {
	return config.App{
		Name:     "agent",
		Type:     agentdeploy.AppType,
		Replicas: config.Replicas{PerNode: true},
		Image:    config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:latest"},
	}
}

var sampleAgentRepo = agentdeploy.Repo{URL: "https://example.test/fleet.git", Ref: "main"}

// deployAgentFixture is a synced fleet with one instance and the agent App,
// a node faked by node, reached through a fake tunnel that records the
// address the route dialed.
type deployAgentFixture struct {
	store   *fakeStore
	tunnels *fakeTunnelSource
	creds   *fakeCredentialStore
	node    *nodeprovisiontest.Server
	dialed  []string
}

func newDeployAgentFixture(t *testing.T, existing ...string) *deployAgentFixture {
	t.Helper()
	inst := sampleSeedInstance()
	f := &deployAgentFixture{
		store: &fakeStore{
			apps:           []config.App{sampleAgentApp()},
			instanceByName: map[string]config.Instance{inst.Name: inst},
		},
		creds: &fakeCredentialStore{},
		node:  nodeprovisiontest.New(t, existing...),
	}
	f.tunnels = &fakeTunnelSource{dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		f.dialed = append(f.dialed, address)
		return f.node.Dial(ctx, network, f.node.Addr())
	}}
	return f
}

func (f *deployAgentFixture) post(t *testing.T, name string, repo agentdeploy.Repo) *httptest.ResponseRecorder {
	t.Helper()
	svc := NewService(nil, f.store, nil, nil, f.tunnels, f.creds)
	svc.SetAgentRepo(repo)
	rec := httptest.NewRecorder()
	NewFromService(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/instances/"+name+"/deploy-agent", nil))
	return rec
}

func decodeDeployAgent(t *testing.T, rec *httptest.ResponseRecorder) deployAgentResponse {
	t.Helper()
	var got deployAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return got
}

func TestInstanceDeployAgentSuccess(t *testing.T) {
	// Another member's agent, as a cluster-wide list shows it, is not this
	// node's.
	f := newDeployAgentFixture(t, "unrelated", "agent-devnode1-g0")

	rec := f.post(t, "devnode0", sampleAgentRepo)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	if got := decodeDeployAgent(t, rec); got.Status != "deployed" || got.Instance != "agent-devnode0-g0" {
		t.Errorf("response = %+v, want deployed agent-devnode0-g0", got)
	}

	created := f.node.Created()
	if len(created) != 1 {
		t.Fatalf("created %d instances, want 1", len(created))
	}
	want := agentdeploy.Desired(sampleAgentApp(), sampleSeedInstance(), sampleAgentRepo)
	if !reflect.DeepEqual(created[0], want) {
		t.Errorf("created %+v, want agentdeploy.Desired's %+v", created[0], want)
	}
	// Spelled out, not only via Desired: the first agent carries the same
	// generation-0 tags as the ones the primary creates on members (#203).
	for key, wantVal := range map[string]string{
		incuslocal.AppTagKey:        "agent",
		incuslocal.GenerationTagKey: "0",
		incuslocal.ImageTagKey:      `{"server":"https://ghcr.io","protocol":"oci","alias":"ehharvey/homelab-ops/agent:latest"}`,
	} {
		if got := created[0].Config[key]; got != wantVal {
			t.Errorf("Config[%q] = %q, want %q", key, got, wantVal)
		}
	}

	wantAddr := net.JoinHostPort("10.100.0.5", seed.IncusHTTPSPort)
	for _, a := range f.dialed {
		if a != wantAddr {
			t.Errorf("dialed %q, want the instance's tunnel address %q", a, wantAddr)
		}
	}

	cred, ok, _ := f.creds.InstanceCredential(context.Background(), "devnode0")
	if !ok {
		t.Fatal("no credential minted for devnode0")
	}
	fp, err := cert.Fingerprint(cred.BootstrapCertPEM)
	if err != nil {
		t.Fatalf("cert.Fingerprint: %v", err)
	}
	if got := f.node.Revoked(); len(got) != 1 || got[0] != fp {
		t.Errorf("revoked = %v, want the minted bootstrap cert %s", got, fp)
	}
}

func TestInstanceDeployAgentAlreadyDeployed(t *testing.T) {
	f := newDeployAgentFixture(t, "agent-devnode0-g1")

	rec := f.post(t, "devnode0", sampleAgentRepo)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if got := decodeDeployAgent(t, rec); got.Status != "already_deployed" || got.Instance != "agent-devnode0-g1" {
		t.Errorf("response = %+v, want already_deployed agent-devnode0-g1", got)
	}
	if n := len(f.node.Created()); n != 0 {
		t.Errorf("created %d instances, want none", n)
	}
	// The list proved the node still trusts the one-shot credential (the
	// agent came from `bootstrap deploy-agent`), so it is spent here too.
	if got := f.node.Revoked(); len(got) != 1 {
		t.Errorf("revoked = %v, want the unused bootstrap cert", got)
	}
}

func TestInstanceDeployAgentSpentCredential409s(t *testing.T) {
	f := newDeployAgentFixture(t)

	if rec := f.post(t, "devnode0", sampleAgentRepo); rec.Code != http.StatusCreated {
		t.Fatalf("first deploy status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
	rec := f.post(t, "devnode0", sampleAgentRepo)

	if rec.Code != http.StatusConflict {
		t.Fatalf("repeat deploy status = %d, want 409; body: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "bootstrap deploy-agent") {
		t.Errorf("409 body %q does not point at `bootstrap deploy-agent`", rec.Body)
	}
}

func TestInstanceDeployAgentErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance string
		setup    func(*deployAgentFixture)
		noRepo   bool
		want     int
	}{
		{name: "unknown instance", instance: "nope", want: http.StatusNotFound},
		{name: "no agent app", setup: func(f *deployAgentFixture) { f.store.apps = nil }, want: http.StatusUnprocessableEntity},
		{name: "instance store error", setup: func(f *deployAgentFixture) { f.store.instanceErr = errors.New("db down") }, want: http.StatusInternalServerError},
		{name: "apps store error", setup: func(f *deployAgentFixture) { f.store.appsErr = errors.New("db down") }, want: http.StatusInternalServerError},
		{name: "credential store error", setup: func(f *deployAgentFixture) { f.creds.readErr = errors.New("db down") }, want: http.StatusInternalServerError},
		{name: "tunnel down", setup: func(f *deployAgentFixture) {
			f.tunnels.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no handshake") }
		}, want: http.StatusBadGateway},
		{name: "create fails", setup: func(f *deployAgentFixture) { f.node.CreateErr = true }, want: http.StatusBadGateway},
		{name: "repo unconfigured", noRepo: true, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeployAgentFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			instance := tc.instance
			if instance == "" {
				instance = "devnode0"
			}
			repo := sampleAgentRepo
			if tc.noRepo {
				repo = agentdeploy.Repo{}
			}

			rec := f.post(t, instance, repo)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestInstanceDeployAgentUnconfigured503s(t *testing.T) {
	for _, tc := range []struct {
		name    string
		store   Store
		tunnels TunnelSource
	}{
		{name: "store", tunnels: &fakeTunnelSource{}},
		{name: "tunnels", store: &fakeStore{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(nil, tc.store, nil, nil, tc.tunnels, &fakeCredentialStore{})
			svc.SetAgentRepo(sampleAgentRepo)
			rec := httptest.NewRecorder()
			NewFromService(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/instances/devnode0/deploy-agent", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
		})
	}
}

func TestInstanceDeployAgentReadsRepoPerRequest(t *testing.T) {
	f := newDeployAgentFixture(t)
	svc := NewService(nil, f.store, nil, nil, f.tunnels, f.creds)
	h := NewFromService(svc)
	svc.SetAgentRepo(sampleAgentRepo) // after the mux is built

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/instances/devnode0/deploy-agent", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}
}
