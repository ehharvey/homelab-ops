// Package configdiff compares a freshly synced fleet Config against the
// previously stored snapshot and reports additions, removals, and changes
// by Network/Instance name. It is warn-only: callers decide what, if
// anything, to do with the Result — this package never mutates state.
package configdiff

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/ehharvey/homelab-ops/internal/config"
)

// NetworkChange holds the before/after pair for a Network name present in
// both the old and new Config but with differing field values.
type NetworkChange struct {
	Name string
	Old  config.Network
	New  config.Network
}

// InstanceChange holds the before/after pair for an Instance name present
// in both the old and new Config but with differing field values.
type InstanceChange struct {
	Name string
	Old  config.Instance
	New  config.Instance
}

// AppChange holds the before/after pair for an App name present in both the
// old and new Config but with differing field values.
type AppChange struct {
	Name string
	Old  config.App
	New  config.App
}

// Result is the outcome of comparing two Configs, keyed by Name.
type Result struct {
	AddedNetworks   []config.Network
	RemovedNetworks []config.Network
	ChangedNetworks []NetworkChange

	AddedInstances   []config.Instance
	RemovedInstances []config.Instance
	ChangedInstances []InstanceChange

	AddedApps   []config.App
	RemovedApps []config.App
	ChangedApps []AppChange
}

// Empty reports whether old and new were identical: no additions,
// removals, or changes on either side.
func (r Result) Empty() bool {
	return len(r.AddedNetworks) == 0 && len(r.RemovedNetworks) == 0 && len(r.ChangedNetworks) == 0 &&
		len(r.AddedInstances) == 0 && len(r.RemovedInstances) == 0 && len(r.ChangedInstances) == 0 &&
		len(r.AddedApps) == 0 && len(r.RemovedApps) == 0 && len(r.ChangedApps) == 0
}

// Lines renders Result as human-readable warning lines, networks before
// instances before apps, added/changed/removed within each. Returns nil if
// Empty().
func (r Result) Lines() []string {
	if r.Empty() {
		return nil
	}

	var lines []string
	for _, n := range r.AddedNetworks {
		lines = append(lines, fmt.Sprintf("+ network %s added", n.Name))
	}
	for _, c := range r.ChangedNetworks {
		lines = append(lines, fmt.Sprintf("~ network %s changed", c.Name))
	}
	for _, n := range r.RemovedNetworks {
		lines = append(lines, fmt.Sprintf("- network %s removed", n.Name))
	}
	for _, i := range r.AddedInstances {
		lines = append(lines, fmt.Sprintf("+ instance %s added", i.Name))
	}
	for _, c := range r.ChangedInstances {
		lines = append(lines, fmt.Sprintf("~ instance %s changed", c.Name))
	}
	for _, i := range r.RemovedInstances {
		lines = append(lines, fmt.Sprintf("- instance %s removed", i.Name))
	}
	for _, a := range r.AddedApps {
		lines = append(lines, fmt.Sprintf("+ app %s added", a.Name))
	}
	for _, c := range r.ChangedApps {
		lines = append(lines, fmt.Sprintf("~ app %s changed", c.Name))
	}
	for _, a := range r.RemovedApps {
		lines = append(lines, fmt.Sprintf("- app %s removed", a.Name))
	}
	return lines
}

// Diff compares old (the last-synced state) against newCfg (the freshly
// pulled state), matching Networks/Instances/Apps by Name. A name present
// in both is "changed" if its full struct value differs (reflect.DeepEqual);
// this intentionally treats DNS/Applications list reordering as a change
// — those are operator-authored YAML lists, config.Parse preserves order
// verbatim, and nothing in the schema declares them sets, so a warn-only
// diff should flag a reorder and let the human judge rather than silently
// deciding order is never meaningful.
//
// A duplicate name within either Config's slice is resolved last-one-wins
// (iterating in order), matching internal/store.Store.Replace's dedup
// semantics — the diff's notion of "the config" should match what
// actually gets stored.
func Diff(old, newCfg config.Config) Result {
	var r Result

	oldNetworks := networksByName(old.Networks)
	newNetworks := networksByName(newCfg.Networks)
	for name, n := range newNetworks {
		old, ok := oldNetworks[name]
		if !ok {
			r.AddedNetworks = append(r.AddedNetworks, n)
			continue
		}
		if !reflect.DeepEqual(old, n) {
			r.ChangedNetworks = append(r.ChangedNetworks, NetworkChange{Name: name, Old: old, New: n})
		}
	}
	for name, n := range oldNetworks {
		if _, ok := newNetworks[name]; !ok {
			r.RemovedNetworks = append(r.RemovedNetworks, n)
		}
	}

	oldInstances := instancesByName(old.Instances)
	newInstances := instancesByName(newCfg.Instances)
	for name, i := range newInstances {
		old, ok := oldInstances[name]
		if !ok {
			r.AddedInstances = append(r.AddedInstances, i)
			continue
		}
		if !reflect.DeepEqual(old, i) {
			r.ChangedInstances = append(r.ChangedInstances, InstanceChange{Name: name, Old: old, New: i})
		}
	}
	for name, i := range oldInstances {
		if _, ok := newInstances[name]; !ok {
			r.RemovedInstances = append(r.RemovedInstances, i)
		}
	}

	oldApps := appsByName(old.Apps)
	newApps := appsByName(newCfg.Apps)
	for name, a := range newApps {
		old, ok := oldApps[name]
		if !ok {
			r.AddedApps = append(r.AddedApps, a)
			continue
		}
		if !reflect.DeepEqual(old, a) {
			r.ChangedApps = append(r.ChangedApps, AppChange{Name: name, Old: old, New: a})
		}
	}
	for name, a := range oldApps {
		if _, ok := newApps[name]; !ok {
			r.RemovedApps = append(r.RemovedApps, a)
		}
	}

	sortByName(r.AddedNetworks, func(n config.Network) string { return n.Name })
	sortByName(r.RemovedNetworks, func(n config.Network) string { return n.Name })
	sortByName(r.ChangedNetworks, func(c NetworkChange) string { return c.Name })
	sortByName(r.AddedInstances, func(i config.Instance) string { return i.Name })
	sortByName(r.RemovedInstances, func(i config.Instance) string { return i.Name })
	sortByName(r.ChangedInstances, func(c InstanceChange) string { return c.Name })
	sortByName(r.AddedApps, func(a config.App) string { return a.Name })
	sortByName(r.RemovedApps, func(a config.App) string { return a.Name })
	sortByName(r.ChangedApps, func(c AppChange) string { return c.Name })

	return r
}

// sortByName sorts s in place by the name extracted via key, giving
// Diff's results (and Lines' rendering of them) a deterministic order
// despite being built from map iteration.
func sortByName[T any](s []T, key func(T) string) {
	sort.Slice(s, func(i, j int) bool { return key(s[i]) < key(s[j]) })
}

func networksByName(networks []config.Network) map[string]config.Network {
	m := make(map[string]config.Network, len(networks))
	for _, n := range networks {
		m[n.Name] = n
	}
	return m
}

func instancesByName(instances []config.Instance) map[string]config.Instance {
	m := make(map[string]config.Instance, len(instances))
	for _, i := range instances {
		m[i.Name] = i
	}
	return m
}

func appsByName(apps []config.App) map[string]config.App {
	m := make(map[string]config.App, len(apps))
	for _, a := range apps {
		m[a.Name] = a
	}
	return m
}
