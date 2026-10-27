package agentdeploy

import (
	"reflect"
	"slices"
	"testing"

	lxcapi "github.com/lxc/incus/v7/shared/api"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/incuslocal"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

func sampleAgentApp() config.App {
	return config.App{
		Name:     "agent",
		Type:     AppType,
		Replicas: config.Replicas{PerNode: true},
		Image:    config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:latest"},
	}
}

func TestFindApp(t *testing.T) {
	agent := sampleAgentApp()
	web := config.App{Name: "web", Type: "web", Replicas: config.Replicas{Count: 2}}
	countedAgent := config.App{Name: "agents", Type: AppType, Replicas: config.Replicas{Count: 3}}

	for _, tc := range []struct {
		name   string
		apps   []config.App
		wantOK bool
	}{
		{name: "no apps"},
		{name: "no agent app", apps: []config.App{web}},
		{name: "agent type but not per-node", apps: []config.App{countedAgent}},
		{name: "found among others", apps: []config.App{web, countedAgent, agent}, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FindApp(tc.apps)
			if ok != tc.wantOK {
				t.Fatalf("FindApp ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Name != agent.Name {
				t.Errorf("FindApp = %q, want %q", got.Name, agent.Name)
			}
		})
	}
}

func TestIsAgentInstance(t *testing.T) {
	app := sampleAgentApp()
	inst := config.Instance{Name: "node0"}
	for name, want := range map[string]bool{
		"agent-node0-g0":   true,
		"agent-node0-g12":  true,
		"agent-node1-g3":   false, // another member's agent, in a cluster-wide list
		"agent-cache-1":    false, // an unrelated instance sharing the prefix
		"agent-node0-gx":   false,
		"agent-node0-g":    false,
		"agent-node0-g0-x": false,
		"agentx-node0":     false,
		"agent":            false,
		"web-1":            false,
	} {
		if got := IsAgentInstance(app, inst, name); got != want {
			t.Errorf("IsAgentInstance(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDesired(t *testing.T) {
	app := sampleAgentApp()
	inst := config.Instance{Name: "node0"}

	got := Desired(app, inst, Repo{URL: "https://example.test/fleet.git", Ref: "prod"})

	if got.Name != "agent-node0-g0" {
		t.Errorf("Name = %q, want agent-node0-g0", got.Name)
	}
	if !got.Start {
		t.Error("Start = false; Incus would create the agent and leave it stopped")
	}
	if got.Type != lxcapi.InstanceTypeContainer {
		t.Errorf("Type = %q, want container", got.Type)
	}
	if want := []string{"default", seed.IncusSocketProfile}; !slices.Equal(got.Profiles, want) {
		t.Errorf("Profiles = %v, want %v", got.Profiles, want)
	}
	wantSource := lxcapi.InstanceSource{Type: "image", Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:latest"}
	if !reflect.DeepEqual(got.Source, wantSource) {
		t.Errorf("Source = %+v, want %+v", got.Source, wantSource)
	}
	for key, want := range map[string]string{
		incuslocal.AppTagKey:              "agent",
		incuslocal.GenerationTagKey:       "0",
		incuslocal.ImageTagKey:            `{"server":"https://ghcr.io","protocol":"oci","alias":"ehharvey/homelab-ops/agent:latest"}`,
		"environment.AGENT_NODE_NAME":     "node0",
		"environment.AGENT_INSTANCE_NAME": "agent-node0-g0",
		"environment.CONFIG_REPO_URL":     "https://example.test/fleet.git",
		"environment.CONFIG_REPO_REF":     "prod",
	} {
		if got.Config[key] != want {
			t.Errorf("Config[%q] = %q, want %q", key, got.Config[key], want)
		}
	}
}

// cmd/agent reads its generation from the name suffix, and #98/#109 key on
// the tag: the two must name the same generation.
func TestDesiredNameAndGenerationTagAgree(t *testing.T) {
	app := config.App{Name: "agent"}
	got := Desired(app, config.Instance{Name: "node0"}, Repo{})
	if want := "agent-node0-g" + got.Config[incuslocal.GenerationTagKey]; got.Name != want {
		t.Errorf("name %q, but the generation tag says %q", got.Name, want)
	}
}

func TestDesiredOmitsEmptyRef(t *testing.T) {
	got := Desired(sampleAgentApp(), config.Instance{Name: "node0"}, Repo{URL: "https://example.test/fleet.git"})
	if _, ok := got.Config["environment.CONFIG_REPO_REF"]; ok {
		t.Error("CONFIG_REPO_REF set for an empty Ref; the agent's own default (main) should apply")
	}
}

// The .app tag is the declared App's name on every member's agent, never a
// per-node value: §25 scopes peers by it (#203).
func TestDesiredAppTagIsSharedAcrossMembers(t *testing.T) {
	app := sampleAgentApp()
	a := Desired(app, config.Instance{Name: "node0"}, Repo{})
	b := Desired(app, config.Instance{Name: "node1"}, Repo{})
	if a.Config[incuslocal.AppTagKey] != "agent" || b.Config[incuslocal.AppTagKey] != "agent" {
		t.Fatalf("app tags = %q, %q; want the App name on both", a.Config[incuslocal.AppTagKey], b.Config[incuslocal.AppTagKey])
	}
	if a.Name == b.Name {
		t.Fatalf("both members' agents are named %q; instance names are cluster-wide", a.Name)
	}
}

func TestImageTagRoundTrips(t *testing.T) {
	for _, ref := range []config.ImageRef{
		{},
		sampleAgentApp().Image,
		{Fingerprint: "abc123"},
		{Server: "https://registry.local:5000", Protocol: "simplestreams", Alias: "a/b:c", Fingerprint: "deadbeef"},
		// Characters JSON would escape by default, and a quote.
		{Server: "https://x.test/?a=1&b=<2>", Alias: `odd"alias\`},
	} {
		tag := ImageTag(ref)
		got, err := ParseImageTag(tag)
		if err != nil {
			t.Errorf("ParseImageTag(ImageTag(%+v) = %q): %v", ref, tag, err)
			continue
		}
		if got != ref {
			t.Errorf("round trip of %+v via %q = %+v", ref, tag, got)
		}
		if again := ImageTag(got); again != tag {
			t.Errorf("not canonical: %q re-serialized as %q", tag, again)
		}
	}
}

func TestImageTagIsLegible(t *testing.T) {
	got := ImageTag(config.ImageRef{Server: "https://x.test/?a=1&b=2", Fingerprint: "f"})
	if want := `{"server":"https://x.test/?a=1&b=2","fingerprint":"f"}`; got != want {
		t.Errorf("ImageTag = %s, want %s", got, want)
	}
}

func TestParseImageTagRejectsWhatImageTagNeverWrites(t *testing.T) {
	for _, s := range []string{
		"",
		"null",
		`"ehharvey/homelab-ops/agent:latest"`,
		`{"alias":"a","tag":"newer-writer"}`,
		`{"alias":"a"} {"alias":"b"}`,
		`{"alias":"a"`,
		`{"alias":1}`,
		// encoding/json alone accepts each of these.
		`{"ALIAS":"x","alias":"y","server":null}`,
		`{"Alias":"a"}`,
		`{"alias":"a","alias":"a"}`,
		`{"server":null,"alias":"a"}`,
		`{"alias":"a","server":"s"}`, // ImageTag writes server first
		`{"alias":"a"} `,
		`{}x`,
	} {
		if ref, err := ParseImageTag(s); err == nil {
			t.Errorf("ParseImageTag(%q) = %+v, want an error", s, ref)
		}
	}
}
