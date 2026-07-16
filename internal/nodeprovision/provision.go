package nodeprovision

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/cert"
)

// DialFunc dials a TCP connection to a node's Incus API — typically
// (*wireguard.Tunnel).DialContext, since a node's tunnel address is only
// reachable through that specific tunnel's in-process virtual network
// stack (see internal/wireguard's package doc).
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// CreateInstance dials addr (a node's Incus API — typically its TunnelIP
// and Incus's HTTPS port) using certPEM/keyPEM (an instance's
// Credential.BootstrapCertPEM/BootstrapKeyPEM), issues one
// POST /1.0/instances for req, waits for the resulting operation to
// finish, and always revokes the bootstrap cert afterward
// (DELETE /1.0/certificates/<fingerprint>) regardless of the create's
// outcome — this credential is a standing full-access credential for as
// long as it stays trusted, so it must not outlive this one call (see
// docs/Decisions.md §4's "no centralized custody of a standing full-access
// credential" principle).
//
// Deliberately does not use lxc/incus/v7/client: that package hardcodes
// its own DialTLSContext (client/util.go's tlsHTTPClient) with no hook to
// redirect it through dial, and pulls in a dependency tree this repo
// otherwise avoids (see internal/flasher's package doc). A plain
// *http.Client against lxc/incus/v7/shared/api types directly follows the
// same "small subpackage, not the client's dependency tree" precedent
// internal/seed already uses for incus.yaml.
func CreateInstance(ctx context.Context, dial DialFunc, addr string, certPEM, keyPEM []byte, req lxcapi.InstancesPost) error {
	client, err := newHTTPClient(dial, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	fingerprint, err := cert.Fingerprint(certPEM)
	if err != nil {
		return fmt.Errorf("compute bootstrap cert fingerprint: %w", err)
	}
	defer revokeCert(ctx, client, addr, fingerprint)

	op, err := createInstance(ctx, client, addr, req)
	if err != nil {
		return fmt.Errorf("create instance %q: %w", req.Name, err)
	}
	if err := waitOperation(ctx, client, addr, op.ID); err != nil {
		return fmt.Errorf("wait for instance %q creation: %w", req.Name, err)
	}
	return nil
}

func newHTTPClient(dial DialFunc, certPEM, keyPEM []byte) (*http.Client, error) {
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse bootstrap cert/key: %w", err)
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: dial,
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				// The node's Incus server cert is self-signed with no
				// well-known CA to chain-verify against, and the seed never
				// tells the web app what that cert will be (it only
				// configures which *client* certs the node trusts) — there
				// is no fingerprint to pin here. Real identity/authorization
				// for this connection comes from two other layers instead:
				// the WireGuard tunnel itself (dial is only reachable
				// through a handshake already authenticated by the node's
				// pre-registered public key) and the client cert below
				// (which Incus's own trust-store preseed governs). This
				// matches every other direct-to-Incus call in this repo
				// (e.g. validate-issue-5.sh's `curl -k`).
				//
				// TOFU-style fingerprint pinning was considered and
				// rejected for this specific call: CreateInstance is a
				// one-shot dial-create-revoke with no repeat connection to
				// protect by pinning. Revisit if #92's agent-deployment
				// flow ends up making repeated calls to the same node over
				// time, where pinning would actually add protection.
				InsecureSkipVerify: true, //nolint:gosec // G402: matches this repo's existing direct-to-Incus trust model // codeql[go/disabled-certificate-check] trust comes from the WireGuard tunnel + client cert, not server-cert chain verification; see comment above
			},
		},
	}, nil
}

func createInstance(ctx context.Context, client *http.Client, addr string, req lxcapi.InstancesPost) (*lxcapi.Operation, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/1.0/instances", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	envelope, err := do(client, httpReq)
	if err != nil {
		return nil, err
	}

	var op lxcapi.Operation
	if err := json.Unmarshal(envelope.Metadata, &op); err != nil {
		return nil, fmt.Errorf("decode operation: %w", err)
	}
	return &op, nil
}

func waitOperation(ctx context.Context, client *http.Client, addr, operationID string) error {
	url := fmt.Sprintf("https://%s/1.0/operations/%s/wait", addr, operationID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build wait request: %w", err)
	}

	envelope, err := do(client, httpReq)
	if err != nil {
		return err
	}

	var final lxcapi.Operation
	if err := json.Unmarshal(envelope.Metadata, &final); err != nil {
		return fmt.Errorf("decode operation: %w", err)
	}
	if final.StatusCode == lxcapi.Failure {
		return fmt.Errorf("operation failed: %s", final.Err)
	}
	return nil
}

// revokeCert deletes the bootstrap cert identified by fingerprint from the
// node's trust store. Best-effort and logged rather than returned: it runs
// from a defer after the create call has already succeeded or failed, and
// a revoke failure shouldn't mask that outcome — but this repo has no
// cert rotation/revocation infra yet (docs/Out of Scope.md), so a logged
// failure here means the cert is left trusted and needs manual cleanup.
func revokeCert(ctx context.Context, client *http.Client, addr, fingerprint string) {
	url := fmt.Sprintf("https://%s/1.0/certificates/%s", addr, fingerprint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		log.Printf("nodeprovision: build revoke request for %s: %v", fingerprint, err)
		return
	}
	if _, err := do(client, httpReq); err != nil {
		log.Printf("nodeprovision: revoke bootstrap cert %s: %v", fingerprint, err)
	}
}

// do sends req and decodes Incus's standard response envelope, mapping an
// "error"-typed envelope (a well-formed Incus error, as opposed to a
// transport failure) to a Go error.
func do(client *http.Client, req *http.Request) (*lxcapi.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response, nothing to flush

	var envelope lxcapi.Response
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode response (http %d): %w", resp.StatusCode, err)
	}
	if envelope.Type == lxcapi.ErrorResponse {
		return nil, fmt.Errorf("incus error: %s", envelope.Error)
	}
	return &envelope, nil
}
