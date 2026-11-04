package configdiff

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

func net(name, cidr string, dns ...string) config.Network {
	n := config.Network{Name: name}
	if cidr != "" {
		n.CIDR = netip.MustParsePrefix(cidr)
	}
	for _, d := range dns {
		n.DNS = append(n.DNS, netip.MustParseAddr(d))
	}
	return n
}

func inst(name, staticIP string, apps ...string) config.Instance {
	i := config.Instance{Name: name, Applications: apps}
	if staticIP != "" {
		i.StaticIP = netip.MustParseAddr(staticIP)
	}
	return i
}

func TestDiffNoChange(t *testing.T) {
	cfg := config.Config{
		Networks:  []config.Network{net("dev-lan", "10.0.0.0/24", "10.0.0.1")},
		Instances: []config.Instance{inst("devnode0", "10.0.0.10", "incus")},
	}

	got := Diff(cfg, cfg)

	if !got.Empty() {
		t.Errorf("Diff(cfg, cfg) = %+v, want Empty()", got)
	}
	if lines := got.Lines(); lines != nil {
		t.Errorf("Lines() = %v, want nil", lines)
	}
}

func TestDiffAdded(t *testing.T) {
	old := config.Config{}
	newCfg := config.Config{
		Networks:  []config.Network{net("dev-lan", "10.0.0.0/24")},
		Instances: []config.Instance{inst("devnode0", "10.0.0.10")},
	}

	got := Diff(old, newCfg)

	want := Result{
		AddedNetworks:  newCfg.Networks,
		AddedInstances: newCfg.Instances,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffRemoved(t *testing.T) {
	old := config.Config{
		Networks:  []config.Network{net("dev-lan", "10.0.0.0/24")},
		Instances: []config.Instance{inst("devnode0", "10.0.0.10")},
	}
	newCfg := config.Config{}

	got := Diff(old, newCfg)

	want := Result{
		RemovedNetworks:  old.Networks,
		RemovedInstances: old.Instances,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffChangedNetwork(t *testing.T) {
	old := config.Config{Networks: []config.Network{net("dev-lan", "10.0.0.0/24")}}
	newCfg := config.Config{Networks: []config.Network{net("dev-lan", "10.0.1.0/24")}}

	got := Diff(old, newCfg)

	want := Result{ChangedNetworks: []NetworkChange{{Name: "dev-lan", Old: old.Networks[0], New: newCfg.Networks[0]}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffChangedInstance(t *testing.T) {
	old := config.Config{Instances: []config.Instance{inst("devnode0", "10.0.0.10")}}
	newCfg := config.Config{Instances: []config.Instance{inst("devnode0", "10.0.0.20")}}

	got := Diff(old, newCfg)

	want := Result{ChangedInstances: []InstanceChange{{Name: "devnode0", Old: old.Instances[0], New: newCfg.Instances[0]}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffListReorderIsChanged(t *testing.T) {
	old := config.Config{
		Networks:  []config.Network{net("dev-lan", "10.0.0.0/24", "10.0.0.1", "10.0.0.2")},
		Instances: []config.Instance{inst("devnode0", "10.0.0.10", "incus", "alloy")},
	}
	newCfg := config.Config{
		Networks:  []config.Network{net("dev-lan", "10.0.0.0/24", "10.0.0.2", "10.0.0.1")},
		Instances: []config.Instance{inst("devnode0", "10.0.0.10", "alloy", "incus")},
	}

	got := Diff(old, newCfg)

	if len(got.ChangedNetworks) != 1 || len(got.ChangedInstances) != 1 {
		t.Errorf("Diff = %+v, want one changed network and one changed instance (reorder should count as a change)", got)
	}
}

func TestDiffDuplicateNameLastOneWins(t *testing.T) {
	old := config.Config{}
	newCfg := config.Config{
		Networks: []config.Network{
			net("dev-lan", "10.0.0.0/24"),
			net("dev-lan", "10.0.1.0/24"),
		},
	}

	got := Diff(old, newCfg)

	want := Result{AddedNetworks: []config.Network{net("dev-lan", "10.0.1.0/24")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v, want %+v (last duplicate wins, matching store.Replace)", got, want)
	}
}

func TestDiffMixed(t *testing.T) {
	old := config.Config{
		Networks: []config.Network{
			net("removed-net", "10.0.0.0/24"),
			net("changed-net", "10.0.1.0/24"),
			net("unchanged-net", "10.0.2.0/24"),
		},
	}
	newCfg := config.Config{
		Networks: []config.Network{
			net("added-net", "10.0.3.0/24"),
			net("changed-net", "10.0.1.1/24"),
			net("unchanged-net", "10.0.2.0/24"),
		},
	}

	got := Diff(old, newCfg)

	want := Result{
		AddedNetworks:   []config.Network{net("added-net", "10.0.3.0/24")},
		RemovedNetworks: []config.Network{net("removed-net", "10.0.0.0/24")},
		ChangedNetworks: []NetworkChange{{
			Name: "changed-net",
			Old:  net("changed-net", "10.0.1.0/24"),
			New:  net("changed-net", "10.0.1.1/24"),
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v, want %+v", got, want)
	}
}

func TestLines(t *testing.T) {
	r := Result{
		AddedNetworks:    []config.Network{net("new-lan", "10.0.0.0/24")},
		ChangedNetworks:  []NetworkChange{{Name: "dev-lan"}},
		RemovedNetworks:  []config.Network{net("old-lan", "10.0.1.0/24")},
		AddedInstances:   []config.Instance{inst("newnode", "10.0.0.20")},
		ChangedInstances: []InstanceChange{{Name: "devnode0"}},
		RemovedInstances: []config.Instance{inst("oldnode", "10.0.1.20")},
	}

	got := r.Lines()

	want := []string{
		"+ network new-lan added",
		"~ network dev-lan changed",
		"- network old-lan removed",
		"+ instance newnode added",
		"~ instance devnode0 changed",
		"- instance oldnode removed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Lines() = %v, want %v", got, want)
	}
}

func app(name, alias string) config.App {
	return config.App{
		Name:     name,
		Type:     "some-renderer",
		Replicas: config.Replicas{Count: 1},
		Image:    config.ImageRef{Alias: alias},
	}
}

func TestDiffAppAdded(t *testing.T) {
	old := config.Config{}
	newCfg := config.Config{Apps: []config.App{app("web-frontend", "some/image:v1")}}

	got := Diff(old, newCfg)

	want := Result{AddedApps: newCfg.Apps}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff() = %+v, want %+v", got, want)
	}
}

func TestDiffAppRemoved(t *testing.T) {
	old := config.Config{Apps: []config.App{app("web-frontend", "some/image:v1")}}
	newCfg := config.Config{}

	got := Diff(old, newCfg)

	want := Result{RemovedApps: old.Apps}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff() = %+v, want %+v", got, want)
	}
}

// An agent image bump is an ordinary App-changed — the superseded
// kind: AgentConfig design would have given it its own diff signal.
func TestDiffAppChangedImage(t *testing.T) {
	old := config.Config{Apps: []config.App{app("agent", "agent:v1")}}
	newCfg := config.Config{Apps: []config.App{app("agent", "agent:v2")}}

	got := Diff(old, newCfg)

	want := Result{ChangedApps: []AppChange{{Name: "agent", Old: old.Apps[0], New: newCfg.Apps[0]}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff() = %+v, want %+v", got, want)
	}
}

// Replicas is a scaling change an operator should see warned, so it must
// register as a change on its own — not just image bumps.
func TestDiffAppChangedReplicas(t *testing.T) {
	oldApp := app("web-frontend", "some/image:v1")
	newApp := oldApp
	newApp.Replicas = config.Replicas{Count: 3}
	old := config.Config{Apps: []config.App{oldApp}}
	newCfg := config.Config{Apps: []config.App{newApp}}

	got := Diff(old, newCfg)

	want := Result{ChangedApps: []AppChange{{Name: "web-frontend", Old: oldApp, New: newApp}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff() = %+v, want %+v", got, want)
	}
}

func TestDiffAppNoChange(t *testing.T) {
	cfg := config.Config{Apps: []config.App{app("agent", "agent:v1")}}

	got := Diff(cfg, cfg)

	if !got.Empty() {
		t.Errorf("Diff(cfg, cfg) = %+v, want Empty()", got)
	}
}

func TestLinesRendersApps(t *testing.T) {
	r := Result{
		AddedApps:   []config.App{app("added-app", "a:v1")},
		ChangedApps: []AppChange{{Name: "changed-app"}},
		RemovedApps: []config.App{app("removed-app", "r:v1")},
	}

	want := []string{
		"+ app added-app added",
		"~ app changed-app changed",
		"- app removed-app removed",
	}
	if got := r.Lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("Lines() = %v, want %v", got, want)
	}
}

// TestDiffExplicitDefaultOSIsUnchanged pins that `os: incusos` and an omitted
// os are the same machine: the store keeps no os column, so a synced Instance
// that writes os: incusos reads back with os omitted, and every later sync
// diffs against that.
func TestDiffExplicitDefaultOSIsUnchanged(t *testing.T) {
	stored := inst("devnode0", "10.0.0.10")
	synced := stored
	synced.OS = config.OSIncusOS

	if got := Diff(config.Config{Instances: []config.Instance{stored}}, config.Config{Instances: []config.Instance{synced}}); !got.Empty() {
		t.Errorf("Diff(os omitted, os: incusos) = %+v, want Empty()", got)
	}

	other := stored
	other.OS = config.OSTalos
	if got := Diff(config.Config{Instances: []config.Instance{stored}}, config.Config{Instances: []config.Instance{other}}); len(got.ChangedInstances) != 1 {
		t.Errorf("Diff(os omitted, os: talos) = %+v, want one ChangedInstance", got)
	}
}

// TestDiffRuntime pins that runtime is compared resolved: an omitted runtime
// and `runtime: incus` are the same machine, in either direction, while incus
// and kubernetes differ.
func TestDiffRuntime(t *testing.T) {
	omitted := inst("devnode0", "10.0.0.10")
	incus := omitted
	incus.Runtime = config.RuntimeIncus
	k8s := omitted
	k8s.Runtime = config.RuntimeKubernetes
	diff := func(a, b config.Instance) Result {
		return Diff(config.Config{Instances: []config.Instance{a}}, config.Config{Instances: []config.Instance{b}})
	}

	if got := diff(omitted, incus); !got.Empty() {
		t.Errorf("Diff(runtime omitted, runtime: incus) = %+v, want Empty()", got)
	}
	if got := diff(incus, omitted); !got.Empty() {
		t.Errorf("Diff(runtime: incus, runtime omitted) = %+v, want Empty()", got)
	}
	if got := diff(incus, k8s); len(got.ChangedInstances) != 1 {
		t.Errorf("Diff(runtime: incus, runtime: kubernetes) = %+v, want one ChangedInstance", got)
	}
	if got := diff(omitted, k8s); len(got.ChangedInstances) != 1 {
		t.Errorf("Diff(runtime omitted, runtime: kubernetes) = %+v, want one ChangedInstance", got)
	}
}
