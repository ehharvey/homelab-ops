// Package agentdeploy builds the Incus instance for an app-manager agent:
// the one code path that constructs an agent's lxcapi.InstancesPost, shared
// by the web app's POST /instances/{name}/deploy-agent route and
// `bootstrap deploy-agent` (#100), and meant for #203's per-member agents
// too.
//
// It stands in for #98's agent renderer, which is paused: a minimal Desired
// derived from the declared agent App's Image, nothing more.
package agentdeploy

import (
	"fmt"
	"strconv"
	"strings"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

// AppType is the agent App's renderer-registry key (`type: agent`).
const AppType = "agent"

// Repo is the config repo the agent syncs from, passed into its environment
// as CONFIG_REPO_URL and CONFIG_REPO_REF (cmd/agent). An empty Ref leaves
// the agent's default, main.
type Repo struct {
	URL string
	Ref string
}

// FindApp returns the declared agent App: the `replicas: per-node` App with
// Type "agent". config.Validate allows at most one.
func FindApp(apps []config.App) (config.App, bool) {
	for _, app := range apps {
		if app.Type == AppType && app.Replicas.PerNode {
			return app, true
		}
	}
	return config.App{}, false
}

// InstanceName is the agent's first instance on inst:
// <app>-<instance>-g0, generation 0 (docs/AppManager.md), e.g.
// agent-node0-g0. cmd/agent reads its generation back from the suffix.
func InstanceName(app config.App, inst config.Instance) string {
	return fmt.Sprintf("%s-%s-g0", app.Name, inst.Name)
}

// IsAgentInstance reports whether name is app's instance for inst, of any
// generation (<app>-<instance>-g<N>): the "an agent is already deployed
// here" check. It matches the whole name, so neither another member's
// agent in a cluster-wide listing nor an unrelated instance that merely
// starts with "<app>-" counts.
func IsAgentInstance(app config.App, inst config.Instance, name string) bool {
	gen, ok := strings.CutPrefix(name, app.Name+"-"+inst.Name+"-g")
	if !ok {
		return false
	}
	_, err := strconv.ParseUint(gen, 10, 64)
	return err == nil
}

// Desired is the InstancesPost for app's generation-0 instance on inst.
//
// It is a container, because the incus-socket profile's proxy device is
// container-only (internal/seed), and it gets that profile, the agent's
// only route to Incus (docs/Decisions.md §27). The AppTagKey tag is what
// makes the agent visible to itself and its peers (internal/incusregistry);
// the agent writes its own user.homelab-ops.agent.* keys. The instance is
// started as part of the create.
//
// No target member: on a standalone node that is the node itself, but on a
// multi-member cluster Incus's scheduler places a target-less create on
// any member. Pinning it there takes POST /1.0/instances?target=<member>,
// which a standalone server rejects, so it is left to #203's per-member
// agents.
func Desired(app config.App, inst config.Instance, repo Repo) lxcapi.InstancesPost {
	name := InstanceName(app, inst)
	cfg := map[string]string{
		incuslocal.AppTagKey:              app.Name,
		"environment.AGENT_NODE_NAME":     inst.Name,
		"environment.AGENT_INSTANCE_NAME": name,
		"environment.CONFIG_REPO_URL":     repo.URL,
	}
	if repo.Ref != "" {
		cfg["environment.CONFIG_REPO_REF"] = repo.Ref
	}
	return lxcapi.InstancesPost{
		Name: name,
		Type: lxcapi.InstanceTypeContainer,
		// POST /1.0/instances only creates unless asked to start too.
		Start: true,
		InstancePut: lxcapi.InstancePut{
			Profiles: []string{"default", seed.IncusSocketProfile},
			Config:   cfg,
		},
		Source: lxcapi.InstanceSource{
			Type:        "image",
			Server:      app.Image.Server,
			Protocol:    app.Image.Protocol,
			Alias:       app.Image.Alias,
			Fingerprint: app.Image.Fingerprint,
		},
	}
}
