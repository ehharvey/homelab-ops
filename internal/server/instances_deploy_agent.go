package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/clusterruntime"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

// deployAgentResponse reports what POST /instances/{name}/deploy-agent did
// on the node: Status is "deployed" (201) or "already_deployed" (200), and
// Instance names the agent instance created or found.
type deployAgentResponse struct {
	Status   string `json:"status"`
	Instance string `json:"instance"`
}

// handleInstanceDeployAgent puts the first app-manager agent on name's node
// (#100, docs/Decisions.md §27), through its cluster runtime's Bootstrap
// (internal/clusterruntime): over the node's WireGuard tunnel, with the
// instance's minted one-shot bootstrap credential, which the create revokes
// afterwards.
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
		rt, err := clusterruntime.ForInstance(inst)
		if err != nil {
			writeDeployAgentError(w, r, name, err)
			return
		}
		apps, err := store.Apps(ctx)
		if err != nil {
			writeDeployAgentError(w, r, name, fmt.Errorf("list apps: %w", err))
			return
		}

		res, err := rt.Bootstrap(ctx, clusterruntime.BootstrapInput{
			Apps:     apps,
			Instance: inst,
			Repo:     *repo,
			Dial:     tunnels.DialContext,
			Addr:     net.JoinHostPort(inst.TunnelIP.String(), seed.IncusHTTPSPort),
			Credential: func(ctx context.Context) ([]byte, []byte, error) {
				cred, err := nodeprovision.EnsureCredential(ctx, creds, name)
				if err != nil {
					return nil, nil, fmt.Errorf("ensure credential: %w", err)
				}
				return cred.BootstrapCertPEM, cred.BootstrapKeyPEM, nil
			},
			OneShot: true,
		})
		if err != nil {
			writeDeployAgentError(w, r, name, err)
			return
		}
		if res.AlreadyDeployed {
			// Logged, not failed: the agent is there either way, and a
			// cert the revoke left trusted needs manual cleanup, as after
			// CreateInstance's.
			if res.RevokeErr != nil {
				log.Printf("deploy agent to %q: revoke unused bootstrap credential: %v", name, res.RevokeErr)
			}
			writeJSON(w, deployAgentResponse{Status: "already_deployed", Instance: res.Agent})
			return
		}
		writeJSONStatus(w, http.StatusCreated, deployAgentResponse{Status: "deployed", Instance: res.Agent})
	}
}

// writeDeployAgentError maps a deploy-agent failure to an HTTP response,
// logging the full error first, in writeResolveSeedError's style: 404 for
// an unknown instance, 422 for an Instance whose runtime this build doesn't
// implement or a fleet with no agent App, 409 for a
// bootstrap credential the node no longer trusts, 502 for an unreachable or
// failing node, 500 for anything else.
func writeDeployAgentError(w http.ResponseWriter, r *http.Request, name string, err error) {
	log.Printf("deploy agent to %q: %v", name, err)
	switch {
	case errors.Is(err, errInstanceNotFound):
		http.NotFound(w, r)
	case errors.Is(err, clusterruntime.ErrNoRuntime):
		// A runtime the store kept but config.Validate would reject.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, clusterruntime.ErrNoAgentApp):
		http.Error(w, clusterruntime.ErrNoAgentApp.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, nodeprovision.ErrUntrusted):
		http.Error(w, "node does not trust this instance's bootstrap credential: it is one-shot, so an earlier deploy-agent call spent it (whether that call succeeded or failed), or the node was never seeded with it; to deploy now, run `bootstrap deploy-agent` with the break-glass cert", http.StatusConflict)
	case errors.Is(err, clusterruntime.ErrNode):
		// The tunnel not up yet, or the node's Incus failing a request, as
		// opposed to a fault in this server.
		http.Error(w, "node incus request failed", http.StatusBadGateway)
	default:
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
