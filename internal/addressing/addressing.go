// Package addressing is the web app's resolve step for network addressing
// (docs/Decisions.md §28, #199): a Network's cidr, gateway,
// dhcp_excluded_range, dns and dhcp.pool come from git (a private config
// repo) or from web app state (the addressing file), exactly one source per
// network. Git always holds each network's name and its dhcp.mode (#490).
//
// The web app's sync is parse → Resolve → config.Validate. Resolve runs the
// checks on the two inputs (all or nothing, one source per network, an entry
// for an undeclared network, a static_ip on a state-addressed network, and the
// CONFIG_REPO_PRIVATE guard) and joins them into one Config. Everything
// downstream (config.Validate's address checks, ipam.Assign, seed.Render)
// sees one resolved Network and doesn't know its mode.
//
// The interim state source is a file of full `kind: Network` documents, named
// by NETWORK_ADDRESSING_PATH. Loading addressing (and pins) through the CLI
// into the encrypted store is #514.
package addressing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configdiff"
)

// File is the addressing file: one or more `kind: Network` documents, each
// carrying a network's full addressing, parsed with config.Parse. It is read
// on every Networks call, so a sync always resolves against the file as it is
// now, never a copy cached from an earlier sync.
type File struct {
	Path string
}

// Networks reads and parses the file. Any kind other than Network is an
// error: the file holds addressing only.
func (f File) Networks(context.Context) ([]config.Network, error) {
	r, err := os.Open(f.Path) //nolint:gosec // G304: the operator's own NETWORK_ADDRESSING_PATH
	if err != nil {
		return nil, fmt.Errorf("open addressing file: %w", err)
	}
	cfg, err := config.Parse(r)
	err = errors.Join(err, r.Close())
	if err != nil {
		return nil, fmt.Errorf("parse addressing file %s: %w", f.Path, err)
	}
	if n := len(cfg.Instances) + len(cfg.Apps) + len(cfg.Designations); n > 0 {
		return nil, fmt.Errorf("addressing file %s: holds %d document(s) of a kind other than Network; it carries network addressing only", f.Path, n)
	}
	return cfg.Networks, nil
}

// Options are the deployment settings Resolve applies.
type Options struct {
	// RepoPrivate allows git-addressed networks (CONFIG_REPO_PRIVATE=true).
	// Repo visibility can't be detected over a plain git transport, so the
	// default rejects git addressing rather than let a public repo publish
	// the LAN's addresses by accident (§28, §29).
	RepoPrivate bool
}

// Resolved is a fleet whose every Network carries addressing from one source.
type Resolved struct {
	Config config.Config
	// StateAddressed names the networks whose addressing came from state, in
	// git's order.
	StateAddressed []string
}

// Rejection is why Resolve refused a fleet, split by what the fix needs, so
// the web app can point the operator at it rather than at the git remote.
type Rejection struct {
	// Git are the faults git's fleet has on its own, whatever this web app's
	// addressing: partial addressing, or a static_ip on a state-addressed
	// network (config.AddressingIssues). The agent's sync
	// (config.ValidateUnresolved) rejects the same commit.
	Git config.Issues
	// Resolve are the faults in joining git with this web app's addressing:
	// git addressing without CONFIG_REPO_PRIVATE, an addressing-file entry
	// that fails CheckEntries or names a network git doesn't declare, a
	// network addressed in both, and a state-addressed network with no
	// entry. Each names its network, and the fix may be in git or in the
	// web app's settings.
	Resolve config.Issues
}

// Empty reports whether Resolve found nothing to reject.
func (r Rejection) Empty() bool { return r.Git.Empty() && r.Resolve.Empty() }

// All is every issue, git's first.
func (r Rejection) All() config.Issues { return slices.Concat(r.Git, r.Resolve) }

// CheckEntries runs the checks on the addressing entries that need no git:
// each has a name, a cidr, and a name no earlier entry has, and none sets
// dhcp.mode, which is git's (#490). Resolve runs
// them, and cmd/web runs them at startup, so a file that fails them stops
// the web app there, naming the setting, rather than failing every sync.
func CheckEntries(state []config.Network) config.Issues {
	issues, _ := entries(state)
	return issues
}

// entries is CheckEntries, plus the index of each entry that has a name no
// earlier entry has, by that name.
func entries(state []config.Network) (config.Issues, map[string]int) {
	var issues config.Issues
	index := make(map[string]int, len(state))
	for i, s := range state {
		path := fmt.Sprintf("addressing[%d]", i)
		switch j, dup := index[s.Name]; {
		case strings.TrimSpace(s.Name) == "":
			issues = append(issues, config.Issue{Path: path + ".name", Message: "must not be empty"})
		case dup:
			issues = append(issues, config.Issue{Path: path + ".name", Message: fmt.Sprintf("network %q is already addressed by addressing[%d]", s.Name, j)})
		default:
			index[s.Name] = i
			if !s.CIDR.IsValid() {
				issues = append(issues, config.Issue{Path: path + ".cidr", Message: fmt.Sprintf("network %q: an addressing entry must set a cidr", s.Name)})
			}
		}
		if s.DHCP.Mode != "" {
			issues = append(issues, config.Issue{Path: path + ".dhcp.mode", Message: fmt.Sprintf("network %q: an addressing entry can't set dhcp.mode: the mode is set in git, on the network's kind: Network; the addressing file carries cidr, gateway, dhcp_excluded_range, dns and dhcp.pool only", s.Name)})
		}
	}
	return issues, index
}

// Resolve joins git's networks with the state addressing in state. It
// rejects, naming the network, everything Rejection lists. A network
// addressed in both is never settled by a precedence rule (#52's stance).
//
// The resolved Config's Networks and Instances are new slices, in git's
// order, so IPAM filling in addresses on the result never writes into git's
// input (a caller holding it would then see a static_ip git never set); Apps
// and Designations are shared. The caller runs config.Validate
// on it next, which checks the resolved addresses (cidr, gateway and range in
// the cidr, a static_ip inside its network's range, the gateway collision of
// #53) the same way for both modes.
func Resolve(git config.Config, state []config.Network, opts Options) (Resolved, Rejection) {
	rej := Rejection{Git: config.AddressingIssues(git)}
	add := func(path, msg string) { rej.Resolve = append(rej.Resolve, config.Issue{Path: path, Message: msg}) }

	gitIndex := make(map[string]int, len(git.Networks))
	for i, n := range git.Networks {
		if _, dup := gitIndex[n.Name]; !dup {
			gitIndex[n.Name] = i
		}
		// A cidr is what git addressing is. A network with only a gateway,
		// range or dns is partial, which is Rejection.Git's, and setting
		// CONFIG_REPO_PRIVATE wouldn't fix it.
		if !opts.RepoPrivate && n.CIDR.IsValid() {
			add(fmt.Sprintf("networks[%d]", i), fmt.Sprintf("network %q is addressed in git, but CONFIG_REPO_PRIVATE is not \"true\": set it if the config repo is private, or move the addressing to the web app's addressing file (NETWORK_ADDRESSING_PATH) and leave only the name in git", n.Name))
		}
	}

	entryIssues, stateIndex := entries(state)
	rej.Resolve = append(rej.Resolve, entryIssues...)
	byName := make(map[string]config.Network, len(stateIndex))
	for i, s := range state {
		if j, ok := stateIndex[s.Name]; !ok || j != i {
			continue // no name, or a duplicate: entries named it
		}
		gi, declared := gitIndex[s.Name]
		switch {
		case !declared:
			add(fmt.Sprintf("addressing[%d].name", i), fmt.Sprintf("network %q is not declared in git: an addressing entry needs a kind: Network of the same name in the config repo", s.Name))
		case !s.CIDR.IsValid():
			// entries named it. It addresses nothing, so it's no second
			// source either.
		case git.Networks[gi].CIDR.IsValid():
			// Partial git addressing (no cidr) isn't a second source:
			// AddressingIssues already names it, and "keep one" would
			// point at addressing git doesn't have.
			add(fmt.Sprintf("networks[%d]", gi), fmt.Sprintf("network %q is addressed both in git and in the addressing file (addressing[%d]): keep one", s.Name, i))
		default:
			byName[s.Name] = s
		}
	}

	resolved := git
	resolved.Networks = make([]config.Network, len(git.Networks))
	resolved.Instances = slices.Clone(git.Instances)
	var stateAddressed []string
	for i, n := range git.Networks {
		resolved.Networks[i] = n
		if !n.Unaddressed() {
			continue
		}
		s, ok := byName[n.Name]
		if !ok {
			if _, entry := stateIndex[n.Name]; !entry {
				add(fmt.Sprintf("networks[%d]", i), fmt.Sprintf("network %q has no addressing: set its cidr (and gateway, dhcp_excluded_range, dns, dhcp.pool) in git, or give it an entry in the web app's addressing file (NETWORK_ADDRESSING_PATH)", n.Name))
			}
			continue
		}
		// Only the addressing comes from the entry: git keeps the name and
		// dhcp.mode, which CheckEntries keeps out of the file.
		resolved.Networks[i] = n.WithAddressingOf(s)
		stateAddressed = append(stateAddressed, n.Name)
	}
	if !rej.Empty() {
		return Resolved{}, rej
	}
	return Resolved{Config: resolved, StateAddressed: stateAddressed}, Rejection{}
}

// Change is one state-addressed network whose addressing a sync loaded for
// the first time, or changed.
type Change struct {
	Network string
	// Message is the line the sync reports, naming the network.
	Message string
}

// Changes reports the state-addressed networks of r that d, configdiff's
// diff of r's Config against the last synced snapshot, adds or changes:
// added first, then changed, each by name, as d has them. configdiff reports
// them as config changes, but can't say the addressing came from the web
// app's state, and an edit to the addressing file makes no commit, so the
// sync reports them itself. On a first sync, with no snapshot, pass the diff
// against an empty Config: every network is then added.
func Changes(d configdiff.Result, r Resolved) []Change {
	var cs []Change
	for _, n := range d.AddedNetworks {
		if slices.Contains(r.StateAddressed, n.Name) {
			cs = append(cs, Change{n.Name, fmt.Sprintf("network %q: addressing loaded from the web app's state: %s", n.Name, describe(n))})
		}
	}
	for _, c := range d.ChangedNetworks {
		// configdiff also counts a change to git's half of the network
		// (dhcp.mode) as a change; only the addressing is the state's.
		if slices.Contains(r.StateAddressed, c.Name) && !sameAddressing(c.Old, c.New) {
			cs = append(cs, Change{c.Name, fmt.Sprintf("network %q: addressing from the web app's state changed: %s, was %s", c.Name, describe(c.New), describe(c.Old))})
		}
	}
	return cs
}

// sameAddressing reports whether a and b carry the same addressing, the
// fields WithAddressingOf copies, whatever else differs: a with b's
// addressing is still a.
func sameAddressing(a, b config.Network) bool {
	return reflect.DeepEqual(a, a.WithAddressingOf(b))
}

// describe renders n's addressing as "cidr X, gateway Y, ...", leaving out
// the fields it doesn't set.
func describe(n config.Network) string {
	var parts []string
	if n.CIDR.IsValid() {
		parts = append(parts, "cidr "+n.CIDR.String())
	}
	if n.Gateway.IsValid() {
		parts = append(parts, "gateway "+n.Gateway.String())
	}
	if r := n.DHCPExcludedRange; r.Start.IsValid() {
		parts = append(parts, "dhcp_excluded_range "+r.Start.String()+"-"+r.End.String())
	}
	if len(n.DNS) > 0 {
		dns := make([]string, len(n.DNS))
		for i, d := range n.DNS {
			dns[i] = d.String()
		}
		parts = append(parts, "dns ["+strings.Join(dns, ", ")+"]")
	}
	if r := n.DHCP.Pool; r.Start.IsValid() {
		parts = append(parts, "dhcp.pool "+r.String())
	}
	if len(parts) == 0 {
		return "no addressing"
	}
	return strings.Join(parts, ", ")
}
