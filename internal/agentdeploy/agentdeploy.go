// Package agentdeploy builds the Incus instance for an app-manager agent:
// the one code path that constructs an agent's lxcapi.InstancesPost, shared
// by the web app's POST /instances/{name}/deploy-agent route,
// `bootstrap deploy-agent` (#100) and the primary agent's per-member creates
// (#203, internal/agent).
//
// It stands in for #98's agent renderer, deferred to Phase 8: a minimal
// Desired derived from the declared agent App's Image, nothing more.
package agentdeploy

import (
	"bytes"
	"encoding/json"
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

// FirstGeneration is the generation of an agent's first instance on a node:
// the one the web route, `bootstrap deploy-agent` and the primary's
// per-member creates (#203) make. Each self-upgrade (#109) creates the next.
const FirstGeneration = 0

// InstanceName is the agent's instance of generation generation on inst:
// <app>-<instance>-g<generation> (docs/AppManager.md), e.g. agent-node0-g0.
// cmd/agent reads its generation back from the suffix, and #109 keys on
// Desired's GenerationTagKey, so both come from the same argument and can't
// disagree.
func InstanceName(app config.App, inst config.Instance, generation int64) string {
	return fmt.Sprintf("%s-%s-g%d", app.Name, inst.Name, generation)
}

// IsAgentInstance reports whether name is app's instance for inst, of any
// generation (<app>-<instance>-g<N>): the "an agent is already deployed
// here" check. It matches the whole name, so neither another member's
// agent in a cluster-wide listing nor an unrelated instance that merely
// starts with "<app>-" counts.
func IsAgentInstance(app config.App, inst config.Instance, name string) bool {
	_, ok := Generation(app, inst, name)
	return ok
}

// Generation is the generation in name's -g<N> suffix, when name is app's
// instance for inst (IsAgentInstance).
func Generation(app config.App, inst config.Instance, name string) (int64, bool) {
	gen, ok := strings.CutPrefix(name, app.Name+"-"+inst.Name+"-g")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(gen, 10, 63) // 63 bits: it fits an int64
	return int64(n), err == nil
}

// Desired is the InstancesPost for app's instance of generation generation on
// inst: FirstGeneration for an agent's first instance there, g+1 for the
// self-upgrade candidate of an instance of generation g (#109).
//
// It is a container, because the incus-socket profile's proxy device is
// container-only (internal/seed), and it gets that profile, the agent's
// only route to Incus (docs/Decisions.md §27). The instance is started as
// part of the create.
//
// Its tags (docs/AppManager.md § What 0.x builds):
//   - AppTagKey is app.Name, the same on every member's agent: it is what
//     makes an agent visible to itself and its peers (internal/incusregistry,
//     docs/Decisions.md §25), so an agent tagged otherwise is neither a peer
//     nor fenced.
//   - GenerationTagKey is generation, as InstanceName's suffix is.
//   - ImageTagKey is app.Image serialized by ImageTag, which the agent's
//     self-upgrade (#109) compares against the declared image.
//
// The agent writes its own user.homelab-ops.agent.* keys.
//
// No target member, so that one value serves every caller: the web route
// and `bootstrap deploy-agent` create on a standalone node, which rejects a
// target, while the primary agent passes target=<member> to
// incuslocal.Client.CreateInstance alongside it (#203).
func Desired(app config.App, inst config.Instance, repo Repo, generation int64) lxcapi.InstancesPost {
	name := InstanceName(app, inst, generation)
	cfg := map[string]string{
		incuslocal.AppTagKey:              app.Name,
		incuslocal.GenerationTagKey:       strconv.FormatInt(generation, 10),
		incuslocal.ImageTagKey:            ImageTag(app.Image),
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

// imageTag is ImageTag's wire shape: config.ImageRef's fields under its YAML
// names, in a fixed order, each omitted when empty.
type imageTag struct {
	Server      string `json:"server,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ImageTag serializes ref as the value of an instance's
// incuslocal.ImageTagKey: a compact JSON object of ref's non-empty fields,
// keyed by their YAML names, e.g.
//
//	{"server":"https://ghcr.io","protocol":"oci","alias":"ehharvey/homelab-ops/agent:latest"}
//
// The output is canonical: equal refs give equal strings, so "has the image
// changed" can compare tags as strings. ParseImageTag reverses it.
func ImageTag(ref config.ImageRef) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "&" and "<" legible in `incus config show`
	// A struct of strings cannot fail to encode.
	_ = enc.Encode(imageTag(ref))
	return strings.TrimSuffix(buf.String(), "\n")
}

// ParseImageTag reads back a value ImageTag wrote. It is strict: it accepts
// only ImageTag's own output, byte for byte, so an unknown, repeated, null or
// differently cased field, a non-object or trailing content is an error
// rather than a partial ref, and a tag a newer writer extended or a hand
// edited reads as "unknown", never as a different image.
//
// The self-upgrade (#109) compares tags as ImageTag strings, which canonical
// output makes exact; this is for reading one back as a ref. The round-trip
// test pins the format every agent created since #203 carries.
func ParseImageTag(s string) (config.ImageRef, error) {
	var t imageTag
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return config.ImageRef{}, fmt.Errorf("image tag %q: %w", s, err)
	}
	// encoding/json alone is lenient: it matches keys case-insensitively,
	// keeps the last of a repeated key, and reads null as "". Re-encoding
	// and comparing rejects all of that, and "null" too.
	ref := config.ImageRef(t)
	if ImageTag(ref) != s {
		return config.ImageRef{}, fmt.Errorf("image tag %q: not as ImageTag writes it", s)
	}
	return ref, nil
}
