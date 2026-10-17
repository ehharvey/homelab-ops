package server

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

// errNoAgentApp marks a deploy-agent request against a fleet that declares
// no agent App (agentdeploy.FindApp): nothing to deploy, a 422 like
// errSeedInvalid.
var errNoAgentApp = errors.New("no agent App declared")

// deployAgentResponse reports what POST /instances/{name}/deploy-agent did
// on the node: Status is "deployed" (201) or "already_deployed" (200), and
// Instance names the agent instance created or found.
type deployAgentResponse struct {
	Status   string `json:"status"`
	Instance string `json:"instance"`
}

// handleInstanceDeployAgent puts the first app-manager agent on name's node
// (#100, docs/Decisions.md §27): over the node's WireGuard tunnel, with the
// instance's minted one-shot bootstrap credential, which CreateInstance
// revokes afterwards.
//
// It lists the node's instances first, so an existing agent instance is
// reported rather than duplicated; the credential is revoked then too, since
// the list just proved the node still trusts it. Because the credential is
// one-shot, any later call is rejected by the node itself; that maps to
// 409, pointing at `bootstrap deploy-agent`, the recovery path that needs
// no web app.
//
// repo is read per request (see Service.SetAgentRepo).
func handleInstanceDeployAgent(store Store, tunnels TunnelSource, creds nodeprovision.CredentialStore, repo *agentdeploy.Repo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		if tunnels == nil {
			http.Error(w, "wireguard not configured (set WIREGUARD_ENDPOINT)", http.StatusServiceUnavailable)
			return
		}
		if repo == nil || repo.URL == "" {
			http.Error(w, "config repo not configured (set CONFIG_REPO_URL)", http.StatusServiceUnavailable)
			return
		}

		ctx := r.Context()
		name := r.PathValue("name")

		inst, ok, err := store.Instance(ctx, name)
		if err != nil {
			writeDeployAgentError(w, r, name, fmt.Errorf("look up instance: %w", err))
			return
		}
		if !ok {
			writeDeployAgentError(w, r, name, errInstanceNotFound)
			return
		}
		apps, err := store.Apps(ctx)
		if err != nil {
			writeDeployAgentError(w, r, name, fmt.Errorf("list apps: %w", err))
			return
		}
		app, ok := agentdeploy.FindApp(apps)
		if !ok {
			writeDeployAgentError(w, r, name, errNoAgentApp)
			return
		}
		cred, err := nodeprovision.EnsureCredential(ctx, creds, name)
		if err != nil {
			writeDeployAgentError(w, r, name, fmt.Errorf("ensure credential: %w", err))
			return
		}
		addr := net.JoinHostPort(inst.TunnelIP.String(), seed.IncusHTTPSPort)

		existing, err := nodeprovision.InstanceNames(ctx, tunnels.DialContext, addr, cred.BootstrapCertPEM, cred.BootstrapKeyPEM)
		if err != nil {
			writeDeployAgentError(w, r, name, fmt.Errorf("%w: list instances: %w", errNodeIncus, err))
			return
		}
		for _, n := range existing {
			if agentdeploy.IsAgentInstance(app, inst, n) {
				// Logged, not failed: the agent is there either way, and a
				// cert the revoke left trusted needs manual cleanup, as
				// after CreateInstance's.
				if err := nodeprovision.RevokeCert(ctx, tunnels.DialContext, addr, cred.BootstrapCertPEM, cred.BootstrapKeyPEM); err != nil {
					log.Printf("deploy agent to %q: revoke unused bootstrap credential: %v", name, err)
				}
				writeJSON(w, deployAgentResponse{Status: "already_deployed", Instance: n})
				return
			}
		}

		req := agentdeploy.Desired(app, inst, *repo)
		if err := nodeprovision.CreateInstance(ctx, tunnels.DialContext, addr, cred.BootstrapCertPEM, cred.BootstrapKeyPEM, req); err != nil {
			writeDeployAgentError(w, r, name, fmt.Errorf("%w: %w", errNodeIncus, err))
			return
		}
		writeJSONStatus(w, http.StatusCreated, deployAgentResponse{Status: "deployed", Instance: req.Name})
	}
}

// errNodeIncus marks a failure talking to the node's Incus — the tunnel not
// up yet, or the create itself failing there — as opposed to a fault in this
// server: a 502.
var errNodeIncus = errors.New("node incus")

// writeDeployAgentError maps a deploy-agent failure to an HTTP response,
// logging the full error first, in writeResolveSeedError's style: 404 for
// an unknown instance, 422 for a fleet with no agent App, 409 for a
// bootstrap credential the node no longer trusts, 502 for an unreachable or
// failing node, 500 for anything else.
func writeDeployAgentError(w http.ResponseWriter, r *http.Request, name string, err error) {
	log.Printf("deploy agent to %q: %v", name, err)
	switch {
	case errors.Is(err, errInstanceNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errNoAgentApp):
		http.Error(w, "no agent App declared (kind: App, type: agent, replicas: per-node)", http.StatusUnprocessableEntity)
	case errors.Is(err, nodeprovision.ErrUntrusted):
		http.Error(w, "node does not trust this instance's bootstrap credential: it is one-shot, so an earlier deploy-agent call spent it (whether that call succeeded or failed), or the node was never seeded with it; to deploy now, run `bootstrap deploy-agent` with the break-glass cert", http.StatusConflict)
	case errors.Is(err, errNodeIncus):
		http.Error(w, "node incus request failed", http.StatusBadGateway)
	default:
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
