package addressing_test

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ehharvey/homelab-ops/internal/addressing"
	"github.com/ehharvey/homelab-ops/internal/cert"
	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configdiff"
	"github.com/ehharvey/homelab-ops/internal/ipam"
	"github.com/ehharvey/homelab-ops/internal/seed"
)

func parse(t *testing.T, doc string) config.Config {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

// addressed is lab's full addressing, used both in git and in state so the
// two modes can be compared like for like.
const addressed = `
cidr: 10.0.2.0/24
gateway: 10.0.2.1
dhcp_excluded_range: 10.0.2.200-10.0.2.250
dns: [10.0.2.1]
`

const instance = `
---
kind: Instance
name: node0
mac: 02:00:00:00:00:01
network: lab
disk: single
nic: single
applications: [incus]
`

func gitAddressed(t *testing.T) config.Config {
	return parse(t, "kind: Network\nname: lab"+addressed+instance)
}

func stateAddressed(t *testing.T) config.Config {
	return parse(t, "kind: Network\nname: lab\n"+instance)
}

func state(t *testing.T) []config.Network {
	return parse(t, "kind: Network\nname: lab"+addressed).Networks
}

// TestResolveRejects covers each rejection the resolve step owns, that
// every message names the network it's about, and which half of the
// Rejection it's in: Git for a fault git has on its own, else Resolve.
func TestResolveRejects(t *testing.T) {
	const homeLAN = `kind: Network
name: home
cidr: 192.168.1.0/24
gateway: 192.168.1.1
`
	tests := []struct {
		name    string
		git     string
		state   string
		private bool
		path    string // the offending issue's path
		want    string // a substring of its message
		inGit   bool   // in Rejection.Git, not Rejection.Resolve
	}{
		{
			name:    "partial addressing in git",
			git:     "kind: Network\nname: lab\ngateway: 10.0.2.1\ndns: [10.0.2.1]\n",
			private: true,
			path:    "networks[0]",
			want:    `network "lab" sets gateway, dns but no cidr`,
			inGit:   true,
		},
		{
			name:    "addressed in both git and state",
			git:     "kind: Network\nname: lab" + addressed,
			state:   "kind: Network\nname: lab" + addressed,
			private: true,
			path:    "networks[0]",
			want:    `network "lab" is addressed both in git and in the addressing file`,
		},
		{
			name:    "state entry for a network git doesn't declare",
			git:     homeLAN,
			state:   "kind: Network\nname: ghost" + addressed,
			private: true,
			path:    "addressing[0].name",
			want:    `network "ghost" is not declared in git`,
		},
		{
			name:  "git static_ip on a state-addressed network",
			git:   "kind: Network\nname: lab\n" + instance + "static_ip: 10.0.2.201\n",
			state: "kind: Network\nname: lab" + addressed,
			path:  "instances[0].static_ip",
			want:  `network "lab" takes its addressing from the web app's state, so instance "node0" can't set a static_ip`,
			inGit: true,
		},
		{
			name: "git addressing without CONFIG_REPO_PRIVATE",
			git:  homeLAN,
			path: "networks[0]",
			want: `network "home" is addressed in git, but CONFIG_REPO_PRIVATE is not "true"`,
		},
		{
			name: "state-addressed network with no state entry",
			git:  "kind: Network\nname: lab\n",
			path: "networks[0]",
			want: `network "lab" has no addressing`,
		},
		{
			name:  "state entry without a cidr",
			git:   "kind: Network\nname: lab\n",
			state: "kind: Network\nname: lab\ngateway: 10.0.2.1\n",
			path:  "addressing[0].cidr",
			want:  `network "lab": an addressing entry must set a cidr`,
		},
		{
			name:  "the same network twice in state (#52)",
			git:   "kind: Network\nname: lab\n",
			state: "kind: Network\nname: lab" + addressed + "---\nkind: Network\nname: lab" + addressed,
			path:  "addressing[1].name",
			want:  `network "lab" is already addressed by addressing[0]`,
		},
		{
			name:  "state entry with no name",
			git:   "kind: Network\nname: lab\n",
			state: "kind: Network\nname: lab" + addressed + "---\nkind: Network\nname: \"\"" + addressed,
			path:  "addressing[1].name",
			want:  "must not be empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var st []config.Network
			if tc.state != "" {
				st = parse(t, tc.state).Networks
			}
			_, rej := addressing.Resolve(parse(t, tc.git), st, addressing.Options{RepoPrivate: tc.private})
			half, other := rej.Resolve, rej.Git
			if tc.inGit {
				half, other = other, half
			}
			found := func(issues config.Issues) bool {
				return slices.ContainsFunc(issues, func(is config.Issue) bool {
					return is.Path == tc.path && strings.Contains(is.Message, tc.want)
				})
			}
			if !found(half) || found(other) {
				t.Errorf("Rejection = %+v, want %s: …%s… in only Git=%v", rej, tc.path, tc.want, tc.inGit)
			}
		})
	}
}

// TestResolvePartialGitAddressingIsOneIssue: a git network with a stray
// gateway but no cidr, and a full state entry, gets only the partial-addressing
// issue. It isn't "addressed both" too: git has no addressing to keep. Nor,
// in a public repo, the CONFIG_REPO_PRIVATE guard: setting it wouldn't fix
// the commit.
func TestResolvePartialGitAddressingIsOneIssue(t *testing.T) {
	git := parse(t, "kind: Network\nname: lab\ngateway: 10.0.2.1\n")
	for _, private := range []bool{true, false} {
		_, rej := addressing.Resolve(git, state(t), addressing.Options{RepoPrivate: private})
		issues := rej.All()
		if len(issues) != 1 || issues[0].Path != "networks[0]" || !strings.Contains(issues[0].Message, `network "lab" sets gateway but no cidr`) {
			t.Errorf("RepoPrivate=%v: issues = %v, want only networks[0]'s partial addressing", private, issues)
		}
	}
}

// TestCheckEntries: the entry checks that need no git, which cmd/web runs at
// startup, find the faults Resolve reports on the entries, and pass a sound
// file whatever git declares.
func TestCheckEntries(t *testing.T) {
	if issues := addressing.CheckEntries(parse(t, "kind: Network\nname: ghost"+addressed).Networks); !issues.Empty() {
		t.Errorf("CheckEntries(one full entry) = %v, want none", issues)
	}
	bad := parse(t, "kind: Network\nname: lab"+addressed+
		"---\nkind: Network\nname: lab"+addressed+
		"---\nkind: Network\nname: \"\""+addressed+
		"---\nkind: Network\nname: nocidr\ngateway: 10.0.3.1\n"+
		"---\nkind: Network\nname: moded"+addressed+"dhcp:\n  mode: full\n").Networks
	var paths []string
	for _, is := range addressing.CheckEntries(bad) {
		paths = append(paths, is.Path)
	}
	if want := []string{"addressing[1].name", "addressing[2].name", "addressing[3].cidr", "addressing[4].dhcp.mode"}; !slices.Equal(paths, want) {
		t.Errorf("CheckEntries paths = %v, want %v", paths, want)
	}
}

// TestResolveDHCP: dhcp.pool is addressing, so it follows the one-source
// rule: from git for a git-addressed network, from the entry for a
// state-addressed one, and partial without a cidr. dhcp.mode is git's: a
// state-addressed network keeps git's mode, and an entry can't set one.
func TestResolveDHCP(t *testing.T) {
	const pool = "dhcp:\n  pool: 10.0.2.100-10.0.2.199\n"
	wantPool := config.Range{Start: netip.MustParseAddr("10.0.2.100"), End: netip.MustParseAddr("10.0.2.199")}

	t.Run("git-addressed pool", func(t *testing.T) {
		git := parse(t, "kind: Network\nname: lab"+addressed+"dhcp:\n  mode: full\n  pool: 10.0.2.100-10.0.2.199\n"+instance)
		r, rej := addressing.Resolve(git, nil, addressing.Options{RepoPrivate: true})
		if !rej.Empty() {
			t.Fatalf("Resolve: %v", rej.All())
		}
		if n := r.Config.Networks[0]; n.DHCP.Pool != wantPool || n.DHCPMode() != config.DHCPFull || len(r.StateAddressed) != 0 {
			t.Errorf("lab = %+v (state-addressed %v), want git's pool and mode", n, r.StateAddressed)
		}
	})

	t.Run("state-addressed pool, git's mode", func(t *testing.T) {
		git := parse(t, "kind: Network\nname: lab\ndhcp:\n  mode: full\n"+instance)
		entry := parse(t, "kind: Network\nname: lab"+addressed+pool).Networks
		r, rej := addressing.Resolve(git, entry, addressing.Options{})
		if !rej.Empty() {
			t.Fatalf("Resolve: %v", rej.All())
		}
		n := r.Config.Networks[0]
		if n.DHCP.Pool != wantPool || n.DHCP.Mode != config.DHCPFull || n.CIDR != entry[0].CIDR {
			t.Errorf("lab = %+v, want the entry's addressing and pool with git's mode full", n)
		}
		if issues := config.Validate(r.Config); !issues.Empty() {
			t.Fatalf("Validate(resolved): %v", issues)
		}
		if err := ipam.Assign(r.Config.Networks, r.Config.Instances, nil); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		if got, want := r.Config.Instances[0].StaticIP, netip.MustParseAddr("10.0.2.100"); got != want {
			t.Errorf("node0 = %v, want %v: full mode draws from the state pool", got, want)
		}
	})

	t.Run("partial pool", func(t *testing.T) {
		git := parse(t, "kind: Network\nname: lab\n"+pool)
		_, rej := addressing.Resolve(git, state(t), addressing.Options{RepoPrivate: true})
		if issues := rej.Git; len(issues) != 1 || !strings.Contains(issues[0].Message, "sets dhcp.pool but no cidr") {
			t.Errorf("Rejection.Git = %v, want the partial pool", issues)
		}
	})

	t.Run("an entry setting dhcp.mode", func(t *testing.T) {
		entry := parse(t, "kind: Network\nname: lab"+addressed+"dhcp:\n  mode: \"off\"\n").Networks
		_, rej := addressing.Resolve(stateAddressed(t), entry, addressing.Options{})
		if issues := rej.Resolve; len(issues) != 1 || issues[0].Path != "addressing[0].dhcp.mode" {
			t.Errorf("Rejection.Resolve = %v, want addressing[0].dhcp.mode", issues)
		}
	})
}

// TestResolveMixedFleet: one network from each source resolves, in git's
// order, with only the state one reported as state-addressed, and the
// address checks then pass on the resolved fleet.
func TestResolveMixedFleet(t *testing.T) {
	git := parse(t, `kind: Network
name: home
cidr: 192.168.1.0/24
gateway: 192.168.1.1
dhcp_excluded_range: 192.168.1.200-192.168.1.250
---
kind: Network
name: lab
---
kind: Instance
name: node0
mac: 02:00:00:00:00:01
network: home
static_ip: 192.168.1.201
disk: single
nic: single
applications: [incus]
---
kind: Instance
name: node1
mac: 02:00:00:00:00:02
network: lab
disk: single
nic: single
applications: [incus]
`)
	r, issues := addressing.Resolve(git, state(t), addressing.Options{RepoPrivate: true})
	if !issues.Empty() {
		t.Fatalf("Resolve: %v", issues)
	}
	if got := []string{r.Config.Networks[0].Name, r.Config.Networks[1].Name}; !reflect.DeepEqual(got, []string{"home", "lab"}) {
		t.Errorf("networks = %v, want git's order [home lab]", got)
	}
	if !reflect.DeepEqual(r.StateAddressed, []string{"lab"}) {
		t.Errorf("StateAddressed = %v, want [lab]", r.StateAddressed)
	}
	if got, want := r.Config.Networks[1], state(t)[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("lab = %+v, want the state entry %+v", got, want)
	}
	if git.Networks[1].CIDR.IsValid() {
		t.Error("Resolve wrote into git's Networks")
	}
	if issues := config.Validate(r.Config); !issues.Empty() {
		t.Errorf("Validate(resolved): %v", issues)
	}
	if err := ipam.Assign(r.Config.Networks, r.Config.Instances, nil); err != nil {
		t.Errorf("ipam.Assign(resolved): %v", err)
	}
	if got, want := r.Config.Instances[1].StaticIP, netip.MustParseAddr("10.0.2.200"); got != want {
		t.Errorf("node1 assigned %v, want the state range's first address %v", got, want)
	}
}

// TestResolveStateOnlyNeedsNoGuard: a public repo (names only) syncs without
// CONFIG_REPO_PRIVATE.
func TestResolveStateOnlyNeedsNoGuard(t *testing.T) {
	if _, issues := addressing.Resolve(stateAddressed(t), state(t), addressing.Options{}); !issues.Empty() {
		t.Fatalf("Resolve: %v", issues)
	}
}

// TestResolvedAddressChecks: the address checks run on the resolved
// networks, so state addressing gets the same gateway, range and
// static-IP checks git addressing does.
func TestResolvedAddressChecks(t *testing.T) {
	tests := []struct {
		name  string
		state string
		path  string
	}{
		{"gateway outside the cidr", "cidr: 10.0.2.0/24\ngateway: 10.9.9.1\n", "networks[0].gateway"},
		{"range outside the cidr", "cidr: 10.0.2.0/24\ndhcp_excluded_range: 10.0.3.1-10.0.3.9\n", "networks[0].dhcp_excluded_range"},
		{"range start after end", "cidr: 10.0.2.0/24\ndhcp_excluded_range: 10.0.2.9-10.0.2.1\n", "networks[0].dhcp_excluded_range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := parse(t, "kind: Network\nname: lab\n"+tc.state).Networks
			r, rej := addressing.Resolve(stateAddressed(t), st, addressing.Options{})
			if !rej.Empty() {
				t.Fatalf("Resolve: %v", rej.All())
			}
			issues := config.Validate(r.Config)
			for _, is := range issues {
				if is.Path == tc.path {
					return
				}
			}
			t.Errorf("Validate(resolved) = %v, want an issue at %s", issues, tc.path)
		})
	}
}

// TestModesRenderIdenticalSeeds: a git-addressed and a state-addressed
// network with identical addressing render byte-identical seeds for the same
// instance, through the same resolve → validate → IPAM → render path a sync
// and the seed route take.
func TestModesRenderIdenticalSeeds(t *testing.T) {
	pair, err := cert.Generate(cert.Options{CommonName: "node0", ValidityDays: 1})
	if err != nil {
		t.Fatalf("cert.Generate: %v", err)
	}
	render := func(git config.Config, st []config.Network) []byte {
		t.Helper()
		r, issues := addressing.Resolve(git, st, addressing.Options{RepoPrivate: true})
		if !issues.Empty() {
			t.Fatalf("Resolve: %v", issues)
		}
		if issues := config.Validate(r.Config); !issues.Empty() {
			t.Fatalf("Validate: %v", issues)
		}
		if err := ipam.Assign(r.Config.Networks, r.Config.Instances, nil); err != nil {
			t.Fatalf("ipam.Assign: %v", err)
		}
		b, err := seed.Render(r.Config.Networks[0], r.Config.Instances[0], pair.CertPEM, nil, seed.Options{})
		if err != nil {
			t.Fatalf("seed.Render: %v", err)
		}
		out, err := yaml.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return out
	}
	fromGit := render(gitAddressed(t), nil)
	fromState := render(stateAddressed(t), state(t))
	if string(fromGit) != string(fromState) {
		t.Errorf("seeds differ:\ngit:\n%s\nstate:\n%s", fromGit, fromState)
	}
	if !strings.Contains(string(fromState), "10.0.2.200/24") {
		t.Errorf("seed lacks the IPAM address 10.0.2.200/24:\n%s", fromState)
	}
}

func TestChanges(t *testing.T) {
	r, rej := addressing.Resolve(stateAddressed(t), state(t), addressing.Options{})
	if !rej.Empty() {
		t.Fatalf("Resolve: %v", rej.All())
	}
	moved := state(t)
	moved[0].Gateway = netip.MustParseAddr("10.0.2.254")
	pooled := state(t)
	pooled[0].DHCP.Pool = config.Range{Start: netip.MustParseAddr("10.0.2.100"), End: netip.MustParseAddr("10.0.2.199")}
	// The snapshot had another dhcp.mode, from git: configdiff calls the
	// network changed, but its addressing isn't.
	remoded := state(t)
	remoded[0].DHCP.Mode = config.DHCPFull
	if r.Config.Networks[0].DHCPMode() == config.DHCPFull {
		t.Fatal("fixture: lab is already dhcp.mode full")
	}
	tests := []struct {
		name  string
		prior []config.Network
		want  []addressing.Change
	}{
		{"first load", nil, []addressing.Change{{"lab", `network "lab": addressing loaded from the web app's state: cidr 10.0.2.0/24, gateway 10.0.2.1, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1]`}}},
		{"unchanged", state(t), nil},
		{"dhcp.mode only", remoded, nil},
		{"changed", moved, []addressing.Change{{"lab", `network "lab": addressing from the web app's state changed: cidr 10.0.2.0/24, gateway 10.0.2.1, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1], was cidr 10.0.2.0/24, gateway 10.0.2.254, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1]`}}},
		{"pool dropped", pooled, []addressing.Change{{"lab", `network "lab": addressing from the web app's state changed: cidr 10.0.2.0/24, gateway 10.0.2.1, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1], was cidr 10.0.2.0/24, gateway 10.0.2.1, dhcp_excluded_range 10.0.2.200-10.0.2.250, dns [10.0.2.1], dhcp.pool 10.0.2.100-10.0.2.199`}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := configdiff.Diff(config.Config{Networks: tc.prior}, r.Config)
			if got := addressing.Changes(d, r); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Changes = %q, want %q", got, tc.want)
			}
		})
	}
	// A git-addressed network's changes are configdiff's, not Changes'.
	g, _ := addressing.Resolve(gitAddressed(t), nil, addressing.Options{RepoPrivate: true})
	if got := addressing.Changes(configdiff.Diff(config.Config{}, g.Config), g); got != nil {
		t.Errorf("Changes(git-addressed) = %q, want none", got)
	}
}

func TestFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) addressing.File {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return addressing.File{Path: p}
	}
	ctx := context.Background()

	got, err := write("ok.yaml", "kind: Network\nname: lab"+addressed).Networks(ctx)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if !reflect.DeepEqual(got, state(t)) {
		t.Errorf("Networks = %+v, want %+v", got, state(t))
	}

	// Read on every call: an edit shows up without anything restarting.
	f := write("ok.yaml", "kind: Network\nname: lab\ncidr: 10.0.9.0/24\n")
	if got, err := f.Networks(ctx); err != nil || got[0].CIDR.String() != "10.0.9.0/24" {
		t.Errorf("after an edit, Networks = %+v, %v; want cidr 10.0.9.0/24", got, err)
	}

	if _, err := write("inst.yaml", "kind: Network\nname: lab"+addressed+instance).Networks(ctx); err == nil || !strings.Contains(err.Error(), "other than Network") {
		t.Errorf("an Instance in the file: err = %v, want a kind error", err)
	}
	if _, err := write("bad.yaml", "kind: Network\nname: lab\ncidr: nope\n").Networks(ctx); err == nil {
		t.Error("an unparsable file: want an error")
	}
	if _, err := (addressing.File{Path: filepath.Join(dir, "missing.yaml")}).Networks(ctx); err == nil {
		t.Error("a missing file: want an error")
	}
}
