package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

const fakeImageContents = "seeded-image-bytes"

// fakeImageBuilder stands in for the real flasher-tool-backed ImageBuilder so
// these tests need no flasher-tool binary or multi-GB base image. It records
// the seedDir it was handed — so a test can prove the handler both wrote the
// seed there (seedWritten) and cleaned it up afterward — and writes a fixed
// marker to outputPath, exercising the handler's open/stat/stream path.
type fakeImageBuilder struct {
	err error

	recordedSeedDir string
	seedWritten     bool
}

func (b *fakeImageBuilder) Build(_ context.Context, seedDir, outputPath string, logs io.Writer) error {
	b.recordedSeedDir = seedDir
	if _, err := os.Stat(filepath.Join(seedDir, "install.yaml")); err == nil {
		b.seedWritten = true
	}
	// Exercise the log-forwarding writer the handler passes in.
	_, _ = io.WriteString(logs, "fake flasher-tool progress\n")
	if b.err != nil {
		return b.err
	}
	return os.WriteFile(outputPath, []byte(fakeImageContents), 0o600)
}

func imageStoreFixture() *fakeStore {
	net := sampleSeedNetwork()
	inst := sampleSeedInstance()
	return &fakeStore{
		networkByName:  map[string]config.Network{net.Name: net},
		instanceByName: map[string]config.Instance{inst.Name: inst},
	}
}

func TestInstanceImageSuccess(t *testing.T) {
	store := imageStoreFixture()
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}
	builder := &fakeImageBuilder{}

	req := httptest.NewRequest(http.MethodGet, "/instances/devnode0/image", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceImage(store, certs, builder, &fakeTunnelSource{}, &fakeCredentialStore{})(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /instances/devnode0/image = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Body.String(); got != fakeImageContents {
		t.Errorf("body = %q, want %q", got, fakeImageContents)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/octet-stream"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="devnode0.img"`; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Length"), "18"; got != want {
		t.Errorf("Content-Length = %q, want %q (len of %q)", got, want, fakeImageContents)
	}

	// The handler must have rendered the seed into the builder's seedDir...
	if !builder.seedWritten {
		t.Error("builder was called before the seed was written to its seedDir")
	}
	// ...and removed that temp dir once the response was fully streamed.
	if _, err := os.Stat(builder.recordedSeedDir); !os.IsNotExist(err) {
		t.Errorf("temp seed dir %q not cleaned up (stat err = %v)", builder.recordedSeedDir, err)
	}
}

func TestInstanceImageUnknownInstance404s(t *testing.T) {
	store := &fakeStore{}
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodGet, "/instances/does-not-exist/image", nil)
	req.SetPathValue("name", "does-not-exist")
	rec := httptest.NewRecorder()

	handleInstanceImage(store, certs, &fakeImageBuilder{}, &fakeTunnelSource{}, &fakeCredentialStore{})(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /instances/does-not-exist/image = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestInstanceImageBuildError502s(t *testing.T) {
	store := imageStoreFixture()
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}
	builder := &fakeImageBuilder{err: errors.New("flasher-tool exploded")}

	req := httptest.NewRequest(http.MethodGet, "/instances/devnode0/image", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceImage(store, certs, builder, &fakeTunnelSource{}, &fakeCredentialStore{})(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("GET /instances/devnode0/image (build error) = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	// Cleanup must still fire on the error path.
	if _, err := os.Stat(builder.recordedSeedDir); !os.IsNotExist(err) {
		t.Errorf("temp seed dir %q not cleaned up after build error (stat err = %v)", builder.recordedSeedDir, err)
	}
}

func TestInstanceImageStoreUnconfigured503s(t *testing.T) {
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodGet, "/instances/devnode0/image", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceImage(nil, certs, &fakeImageBuilder{}, &fakeTunnelSource{}, &fakeCredentialStore{})(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /instances/devnode0/image (nil store) = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestInstanceImageCertSourceUnconfigured503s(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/instances/devnode0/image", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceImage(&fakeStore{}, nil, &fakeImageBuilder{}, &fakeTunnelSource{}, &fakeCredentialStore{})(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /instances/devnode0/image (nil cert source) = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestInstanceImageBuilderUnconfigured503s(t *testing.T) {
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodGet, "/instances/devnode0/image", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceImage(&fakeStore{}, certs, nil, &fakeTunnelSource{}, &fakeCredentialStore{})(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /instances/devnode0/image (nil builder) = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestInstanceImageTunnelUnconfigured503s(t *testing.T) {
	certs := fakeCertSource{pem: sampleClientCertPEM(t)}

	req := httptest.NewRequest(http.MethodGet, "/instances/devnode0/image", nil)
	req.SetPathValue("name", "devnode0")
	rec := httptest.NewRecorder()

	handleInstanceImage(&fakeStore{}, certs, &fakeImageBuilder{}, nil, nil)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /instances/devnode0/image (nil tunnel source) = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
