package nodeprovision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision/nodeprovisiontest"
)

func writeEnvelope(w http.ResponseWriter, resp lxcapi.Response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func sampleBootstrapCredential(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	pair, err := cert.Generate(cert.Options{CommonName: "bootstrap@node0", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	return pair.CertPEM, pair.KeyPEM
}

func TestCreateInstanceSuccess(t *testing.T) {
	f := nodeprovisiontest.New(t)
	certPEM, keyPEM := sampleBootstrapCredential(t)

	err := CreateInstance(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	wantFingerprint, err := cert.Fingerprint(certPEM)
	if err != nil {
		t.Fatalf("cert.Fingerprint: %v", err)
	}
	if got := f.Revoked(); len(got) != 1 || got[0] != wantFingerprint {
		t.Errorf("revoked fingerprints = %v, want [%s]", got, wantFingerprint)
	}
}

func TestCreateInstanceCreateFailure(t *testing.T) {
	f := nodeprovisiontest.New(t)
	f.CreateErr = true
	certPEM, keyPEM := sampleBootstrapCredential(t)

	err := CreateInstance(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err == nil {
		t.Fatal("CreateInstance with a failing create call = nil error, want non-nil")
	}
	// The cert must still be revoked even though creation failed — it's a
	// deferred call, not conditional on success.
	if len(f.Revoked()) != 1 {
		t.Error("CreateInstance did not revoke the bootstrap cert after a failed create")
	}
}

func TestCreateInstanceOperationFailure(t *testing.T) {
	f := nodeprovisiontest.New(t)
	f.WaitStatus = lxcapi.Failure
	certPEM, keyPEM := sampleBootstrapCredential(t)

	err := CreateInstance(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err == nil {
		t.Fatal("CreateInstance with a failed operation = nil error, want non-nil")
	}
	if len(f.Revoked()) != 1 {
		t.Error("CreateInstance did not revoke the bootstrap cert after an operation failure")
	}
}

func TestCreateInstanceNoRevokeNeverRevokes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*nodeprovisiontest.Server)
		wantErr bool
	}{
		{name: "success", setup: func(*nodeprovisiontest.Server) {}},
		{name: "create failure", setup: func(f *nodeprovisiontest.Server) { f.CreateErr = true }, wantErr: true},
		{name: "operation failure", setup: func(f *nodeprovisiontest.Server) { f.WaitStatus = lxcapi.Failure }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := nodeprovisiontest.New(t)
			tc.setup(f)
			certPEM, keyPEM := sampleBootstrapCredential(t)

			err := CreateInstanceNoRevoke(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("CreateInstanceNoRevoke = %v, wantErr %v", err, tc.wantErr)
			}
			if got := f.Revoked(); len(got) != 0 {
				t.Errorf("revoked fingerprints = %v, want none (a standing cert must never be revoked)", got)
			}
		})
	}
}

func TestInstanceNames(t *testing.T) {
	f := nodeprovisiontest.New(t, "web-1", "agent-node0-g0")
	certPEM, keyPEM := sampleBootstrapCredential(t)

	got, err := InstanceNames(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM)
	if err != nil {
		t.Fatalf("InstanceNames: %v", err)
	}
	if want := []string{"web-1", "agent-node0-g0"}; !slices.Equal(got, want) {
		t.Errorf("InstanceNames = %v, want %v", got, want)
	}
	if len(f.Revoked()) != 0 {
		t.Error("InstanceNames revoked the cert; it must be read-only")
	}
}

func TestInstanceNamesAfterRevokeIsUntrusted(t *testing.T) {
	f := nodeprovisiontest.New(t)
	certPEM, keyPEM := sampleBootstrapCredential(t)
	if err := CreateInstance(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	_, err := InstanceNames(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM)
	if !errors.Is(err, ErrUntrusted) {
		t.Errorf("InstanceNames with a revoked cert = %v, want ErrUntrusted", err)
	}
}

// The web route passes its request's ctx, which the client's disconnect
// cancels mid-create; the revoke must still reach the node.
func TestCreateInstanceRevokesAfterCallerCancels(t *testing.T) {
	f := nodeprovisiontest.New(t)
	certPEM, keyPEM := sampleBootstrapCredential(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.OnWait = func(r *http.Request) {
		cancel()
		<-r.Context().Done() // the client has given up on this wait
	}

	err := CreateInstance(ctx, f.Dial, f.Addr(), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateInstance = %v, want context.Canceled", err)
	}
	if got := f.Revoked(); len(got) != 1 {
		t.Errorf("revoked fingerprints = %v, want the bootstrap cert's despite the cancelled ctx", got)
	}
}

func TestRevokeCert(t *testing.T) {
	f := nodeprovisiontest.New(t)
	certPEM, keyPEM := sampleBootstrapCredential(t)

	if err := RevokeCert(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM); err != nil {
		t.Fatalf("RevokeCert: %v", err)
	}
	wantFingerprint, err := cert.Fingerprint(certPEM)
	if err != nil {
		t.Fatalf("cert.Fingerprint: %v", err)
	}
	if got := f.Revoked(); len(got) != 1 || got[0] != wantFingerprint {
		t.Errorf("revoked fingerprints = %v, want [%s]", got, wantFingerprint)
	}
	if err := RevokeCert(context.Background(), f.Dial, f.Addr(), certPEM, keyPEM); !errors.Is(err, ErrUntrusted) {
		t.Errorf("second RevokeCert = %v, want ErrUntrusted", err)
	}
}

func TestCreateInstanceRevokeFailureDoesNotMaskSuccess(t *testing.T) {
	certPEM, keyPEM := sampleBootstrapCredential(t)

	// A bespoke server (rather than nodeprovisiontest.Server) whose create/wait
	// path succeeds normally but whose DELETE always 500s, so the revoke
	// step specifically fails while the create itself does not.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		op := lxcapi.Operation{ID: "OP1", StatusCode: lxcapi.Running}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.AsyncResponse, Metadata: meta})
	})
	mux.HandleFunc("GET /1.0/operations/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		op := lxcapi.Operation{ID: r.PathValue("id"), StatusCode: lxcapi.Success}
		meta, _ := json.Marshal(op)
		writeEnvelope(w, lxcapi.Response{Type: lxcapi.SyncResponse, Metadata: meta})
	})
	mux.HandleFunc("DELETE /1.0/certificates/{fingerprint}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, "synthetic server error")
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}

	err := CreateInstance(context.Background(), dial, strings.TrimPrefix(srv.URL, "https://"), certPEM, keyPEM, lxcapi.InstancesPost{Name: "agent"})
	if err != nil {
		t.Fatalf("CreateInstance = %v, want nil (a revoke failure must not mask a successful create)", err)
	}
}
