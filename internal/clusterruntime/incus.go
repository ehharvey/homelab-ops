package clusterruntime

import (
	"context"
	"fmt"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
)

// incus is the Incus runtime: the app-manager agent's lifecycle. Its Join,
// Upgrade and Health run the agent's own passes as they are; it's what a nil
// agent.Agent.Runtime runs, so internal/agent's tests cover them.
type incus struct{}

func (incus) Name() string { return config.RuntimeIncus }

// Join runs the leader's per-member agent creates (agent.ensureAgents).
func (incus) Join(p LeaderPass) { p.EnsureAgents() }

// Upgrade runs the blue-green agent self-upgrade (agent.upgradeAgents).
func (incus) Upgrade(p LeaderPass) (drained bool, holds []string) { return p.UpgradeAgents() }

// Health runs the candidate's sustained-healthy check
// (agent.sustainedHealthy).
func (incus) Health(c Candidate) bool { return c.SustainedHealthy() }

// Bootstrap creates the agent App's generation 0 on in.Instance's node
// (#100, §27), unless one of that node's agent instances is already there.
func (incus) Bootstrap(ctx context.Context, in BootstrapInput) (BootstrapResult, error) {
	app, ok := agentdeploy.FindApp(in.Apps)
	if !ok {
		return BootstrapResult{}, ErrNoAgentApp
	}
	certPEM, keyPEM, err := in.Credential(ctx)
	if err != nil {
		return BootstrapResult{}, err
	}

	existing, err := nodeprovision.InstanceNames(ctx, in.Dial, in.Addr, certPEM, keyPEM)
	if err != nil {
		return BootstrapResult{}, nodeError{fmt.Errorf("list instances on %s: %w", in.Addr, err)}
	}
	for _, name := range existing {
		if agentdeploy.IsAgentInstance(app, in.Instance, name) {
			res := BootstrapResult{Agent: name, AlreadyDeployed: true}
			if in.OneShot {
				res.RevokeErr = nodeprovision.RevokeCert(ctx, in.Dial, in.Addr, certPEM, keyPEM)
			}
			return res, nil
		}
	}

	req := agentdeploy.Desired(app, in.Instance, in.Repo, agentdeploy.FirstGeneration)
	create := nodeprovision.CreateInstanceNoRevoke
	if in.OneShot {
		create = nodeprovision.CreateInstance
	}
	if err := create(ctx, in.Dial, in.Addr, certPEM, keyPEM, req); err != nil {
		return BootstrapResult{}, nodeError{err}
	}
	return BootstrapResult{Agent: req.Name}, nil
}
