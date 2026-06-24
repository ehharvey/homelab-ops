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
