package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

type fakeCertSource struct {
	pem []byte
	err error
}

func (f fakeCertSource) ClientCertPEM(_ context.Context) ([]byte, error) {
	return f.pem, f.err
}

func sampleSeedNetwork() config.Network {
	return config.Network{
		Name:    "dev-lan",
		CIDR:    prefix("10.0.0.0/24"),
		Gateway: addr("10.0.0.1"),
	}
}

func sampleSeedInstance() config.Instance {
	return config.Instance{
		Name:         "devnode0",
		MAC:          "aa:bb:cc:dd:ee:00",
		Network:      "dev-lan",
		StaticIP:     addr("10.0.0.210"),
		Disk:         "single",
		NIC:          "single",
		Applications: []string{"incus"},
	}
}

func sampleClientCertPEM(t *testing.T) []byte {
	t.Helper()
	pair, err := cert.Generate(cert.Options{CommonName: "validate-issue-36", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	return pair.CertPEM
}

func TestInstanceSeedSuccess(t *testing.T) {
	net := sampleSeedNetwork()
	inst := sampleSeedInstance()
	certPEM := sampleClientCertPEM(t)

	store := &fakeStore{
		networkByName:  map[string]config.Network{net.Name: net},
		instanceByName: map[string]config.Instance{inst.Name: inst},
	}
	certs := fakeCertSource{pem: certPEM}

	req := httptest.NewRequest(http.MethodPost, "/instances/devnode0/seed", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceSeed(store, certs)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /instances/devnode0/seed = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got instanceSeedResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	wantBundle, err := seed.Render(net, inst, certPEM, seed.Options{})
	if err != nil {
		t.Fatalf("seed.Render: %v", err)
	}

	if got.InstallYAML == "" || got.NetworkYAML == "" || got.ApplicationsYAML == "" || got.IncusYAML == "" {
		t.Fatalf("response has an empty field: %+v", got)
	}

	wantInstall := yamlMarshal(t, wantBundle.Install)
	if got.InstallYAML != wantInstall {
		t.Errorf("install_yaml = %q, want %q", got.InstallYAML, wantInstall)
	}
	wantNetwork := yamlMarshal(t, wantBundle.Network)
	if got.NetworkYAML != wantNetwork {
		t.Errorf("network_yaml = %q, want %q", got.NetworkYAML, wantNetwork)
	}
	wantApplications := yamlMarshal(t, wantBundle.Applications)
	if got.ApplicationsYAML != wantApplications {
		t.Errorf("applications_yaml = %q, want %q", got.ApplicationsYAML, wantApplications)
	}
	wantIncus := yamlMarshal(t, wantBundle.Incus)
	if got.IncusYAML != wantIncus {
		t.Errorf("incus_yaml = %q, want %q", got.IncusYAML, wantIncus)
	}
}

func yamlMarshal(t *testing.T, v any) string {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	return string(out)
}

func TestInstanceSeedUnknownInstance404s(t *testing.T) {
	store := &fakeStore{}
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodPost, "/instances/does-not-exist/seed", nil)
	req.SetPathValue("name", "does-not-exist")
	rec := httptest.NewRecorder()

	handleInstanceSeed(store, certs)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /instances/does-not-exist/seed = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestInstanceSeedMissingNetwork422s(t *testing.T) {
	inst := sampleSeedInstance()
	store := &fakeStore{
		instanceByName: map[string]config.Instance{inst.Name: inst},
		// dev-lan deliberately absent from networkByName.
	}
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodPost, "/instances/devnode0/seed", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceSeed(store, certs)(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /instances/devnode0/seed (missing network) = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestInstanceSeedRenderRejection422s(t *testing.T) {
	net := sampleSeedNetwork()
	inst := sampleSeedInstance()
	inst.Disk = "multi" // unsupported in v1, see seed.Render

	store := &fakeStore{
		networkByName:  map[string]config.Network{net.Name: net},
		instanceByName: map[string]config.Instance{inst.Name: inst},
	}
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodPost, "/instances/devnode0/seed", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceSeed(store, certs)(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /instances/devnode0/seed (unsupported disk) = %d, want %d", rec.Code, http.StatusUnprocessableEntity)
	}
}

func TestInstanceSeedStoreUnconfigured503s(t *testing.T) {
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodPost, "/instances/devnode0/seed", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceSeed(nil, certs)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /instances/devnode0/seed (nil store) = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestInstanceSeedCertSourceUnconfigured503s(t *testing.T) {
	store := &fakeStore{}

	req := httptest.NewRequest(http.MethodPost, "/instances/devnode0/seed", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceSeed(store, nil)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /instances/devnode0/seed (nil cert source) = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
