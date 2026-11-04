// Package clusterruntime is the cluster-runtime seam (docs/Decisions.md §31):
// one Runtime per cluster runtime a fleet can run, chosen by its Instances'
// runtime field (config.Instance.ClusterRuntime). A Runtime bootstraps a
// cluster, joins its members, upgrades it and reports its health.
//
// Incus is the only runtime, and what it bootstraps, joins and upgrades is
// the app-manager agent: its lifecycle is all the Incus runtime does today.
// It wraps internal/agent, internal/agentdeploy and internal/nodeprovision in
// place rather than moving them, so the agent behaves exactly as before the
// seam existed (proven by the agent and deploy-agent validate scripts).
// incusregistry, incuslocal and nodeprovision are its internals and stay
// direct.
//
// The dependency points one way: internal/agent imports this package and
// hands Join, Upgrade and Health its passes (LeaderPass, Candidate), so the
// web app and the bootstrap CLI, which only Bootstrap, don't link the agent.
//
// Two things stay outside the seam. The Incus host cluster's own formation:
// the seed renders the one-member bootstrap behind the host-OS seam
// (internal/hostos), and member join is #180's, which goes behind Join when
// it lands. And the agent loop around Join and Upgrade: leaderWork's order,
// its pass timeout and §25's leader election and acting-flag gating stay in
// internal/agent, whatever runtime it's given (agent.Agent.Runtime).
//
// The interface is what the Incus runtime does, not a guess at Kubernetes'
// needs (§31): Join, Upgrade and Health take the agent's leader passes
// because that is what Incus runs. The Kubernetes runtime (Phase 6+) reshapes
// them around its real behaviour, as hostos gains an upgrade operation only
// with Talos.
package clusterruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/ehharvey/homelab-ops/internal/agentdeploy"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
)

// Runtime is one cluster runtime's implementation of the seam.
type Runtime interface {
	// Name is the config.Instance runtime value this runtime serves.
	Name() string
	// Bootstrap puts the first agent on in.Instance's node: once per
	// cluster at bring-up, or to recover one that lost every agent
	// (docs/Decisions.md §27). A node that already has one is reported,
	// not given a second.
	Bootstrap(ctx context.Context, in BootstrapInput) (BootstrapResult, error)
	// Join brings the cluster's declared members that lack an agent into
	// the agent's cluster: the leader's create of each one (#203). It runs
	// within the leader's tick, on p's read of git and the cluster.
	Join(p LeaderPass)
	// Upgrade moves the cluster's agents to the image git declares, blue-
	// green (#109). drained says this agent just handed over to its
	// candidate; holds are the upgrades held at their attempt cap.
	Upgrade(p LeaderPass) (drained bool, holds []string)
	// Health reports whether an upgrade candidate has been healthy long
	// enough to replace the agent it upgrades, keeping its healthy streak.
	Health(c Candidate) bool
}

// LeaderPass is one agent leader tick's work, on one read of git and the
// cluster and within one pass timeout, as Join and Upgrade get it from
// internal/agent (agent.LeaderPass). Its methods are the agent's own passes.
type LeaderPass interface {
	// EnsureAgents creates the agent on every declared, Online cluster
	// member that has none (#203).
	EnsureAgents()
	// UpgradeAgents replaces every host's agent blue-green when the agent
	// App's image changes in git (#109). It reports drained when this
	// agent just drained for its candidate, and holds for its status.
	UpgradeAgents() (drained bool, holds []string)
}

// Candidate is one host's self-upgrade candidate, as Health gets it from
// internal/agent (agent.Candidate).
type Candidate interface {
	// SustainedHealthy keeps the candidate's healthy streak (on its
	// incusregistry.KeyHeartbeat) on the upgrading agent's own instance,
	// and reports whether it has held for the upgrade's minimum.
	SustainedHealthy() bool
}

// runtimes holds every registered Runtime, keyed by its name. A runtime lands
// together with its config.RuntimeImplemented entry;
// TestRuntimesMatchConfig keeps the two in step.
var runtimes = map[string]Runtime{
	config.RuntimeIncus: Incus,
}

// Incus is the Incus runtime. Callers dispatch through ForInstance; it's
// exported, as hostos.IncusOS is, for one that has no Instance to dispatch
// on: cmd/agent, which is the Incus runtime's agent whatever it has synced.
var Incus Runtime = incus{}

// ErrNoRuntime is For's error for a runtime with no implementation in this
// build. Test it with errors.Is.
var ErrNoRuntime = errors.New("no cluster runtime")

// For returns the Runtime named name, a config.Instance runtime value.
// config.Validate rejects a runtime with no implementation, so this fails,
// with ErrNoRuntime, only for a config that skipped validation.
func For(name string) (Runtime, error) {
	r, ok := runtimes[name]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrNoRuntime, name)
	}
	return r, nil
}

// ForInstance returns the Runtime of inst's cluster
// (config.Instance.ClusterRuntime, which defaults an omitted runtime).
// config.Validate holds every Instance of a fleet to one runtime, so any of
// them names the cluster's.
func ForInstance(inst config.Instance) (Runtime, error) {
	return For(inst.ClusterRuntime())
}

// BootstrapInput is everything Bootstrap needs for one node.
type BootstrapInput struct {
	// Apps are the fleet's declared Apps. The Incus runtime deploys the
	// agent App among them (agentdeploy.FindApp).
	Apps []config.App
	// Instance is the node to bootstrap.
	Instance config.Instance
	// Repo is the config repo the agent syncs.
	Repo agentdeploy.Repo
	// Dial and Addr reach the node's Incus API: through its WireGuard
	// tunnel (the web app) or directly over the LAN (bootstrap
	// deploy-agent).
	Dial nodeprovision.DialFunc
	Addr string
	// Credential returns the client cert and key to authenticate to the
	// node with. Bootstrap calls it only once it has found the agent App,
	// so a fleet that declares none fails before any credential is read or
	// minted.
	Credential func(ctx context.Context) (certPEM, keyPEM []byte, err error)
	// OneShot says Credential is the node's one-shot bootstrap credential
	// (the web app's), which Bootstrap revokes once used: by the create,
	// or, when the agent is already there, after the listing that proved
	// the node still trusts it. Otherwise it's the break-glass cert, which
	// is never revoked.
	OneShot bool
}

// BootstrapResult is what Bootstrap did.
type BootstrapResult struct {
	// Agent is the agent instance created, or the one found.
	Agent string
	// AlreadyDeployed says Agent was there already, and nothing was
	// created.
	AlreadyDeployed bool
	// RevokeErr is a failed revoke of an unused OneShot credential, when
	// the agent was already there. It doesn't fail Bootstrap, since the
	// agent is there either way, but the cert it left trusted needs manual
	// cleanup, as after a create's failed revoke.
	RevokeErr error
}

// ErrNoAgentApp is Bootstrap's error for a fleet that declares no agent App:
// there is nothing to deploy.
var ErrNoAgentApp = fmt.Errorf("no agent App declared (kind: App, type: %s, replicas: per-node)", agentdeploy.AppType)

// ErrNode marks a Bootstrap error from nodeprovision's requests to the node:
// reaching it, or its Incus refusing or failing a request
// (nodeprovision.ErrUntrusted among them). nodeprovision doesn't tell those
// apart from its own local faults before a request goes out, such as a
// cert and key it can't build a client from, so ErrNode marks those too, as
// the web route's 502 did before this seam. It doesn't mark ErrNoAgentApp
// or a failed Credential. Test it with errors.Is.
var ErrNode = errors.New("node")

// nodeError marks err as ErrNode without changing its message.
type nodeError struct{ err error }

func (e nodeError) Error() string        { return e.err.Error() }
func (e nodeError) Unwrap() error        { return e.err }
func (e nodeError) Is(target error) bool { return target == ErrNode }
