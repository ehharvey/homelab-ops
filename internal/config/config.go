// Package config parses the bootstrap CLI's k8s-style fleet definition
// format: one or more YAML documents, each discriminated by a `kind:` field,
// into plain Go objects. Parse performs structural validation only (strict
// unknown-field detection); address fields are typed with net/netip, so
// their *syntactic* well-formedness is checked at parse time via
// encoding.TextUnmarshaler (which yaml.v3 honors — see
// ExampleNetwork_yamlUnmarshal), as are dhcp.mode and dhcp.pool, and an
// Instance's mac is checked with ParseMAC. Cross-field *semantic* validation
// (gateway-in-CIDR, range-in-CIDR, etc.) is a separate pass, Validate.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Network is a parsed `kind: Network` document. The address fields are typed
// with net/netip rather than strings: yaml.v3 honors encoding.TextUnmarshaler,
// so each parses (and is syntactically validated) straight from the YAML, and
// an empty (`gateway: ""`) or omitted optional lands as a zero value with
// IsValid()==false and no error. See ExampleNetwork_yamlUnmarshal.
//
// The addressing fields (cidr, gateway, dhcp_excluded_range, dns,
// dhcp.pool) are optional in a fleet the web app syncs (docs/Decisions.md
// §28, #199). A Network that sets cidr is git-addressed, and its other
// fields stay optional as they always were. One that sets none of them is
// state-addressed: the web app's resolve step (internal/addressing) fills its
// addressing in from the addressing file before Validate runs. Anything in
// between (a gateway, range, dns or pool without a cidr) is partial, and
// AddressingIssues rejects it. Validate itself still requires a cidr, so a
// local file (bootstrap render-seed, deploy-agent) and a resolved fleet carry
// full Networks. dhcp.mode isn't addressing: it always comes from git.
type Network struct {
	Name              string       `yaml:"name"`
	CIDR              netip.Prefix `yaml:"cidr"`
	Gateway           netip.Addr   `yaml:"gateway"`
	DHCPExcludedRange Range        `yaml:"dhcp_excluded_range"`
	DNS               []netip.Addr `yaml:"dns"`
	DHCP              DHCP         `yaml:"dhcp"`
}

// DHCP is a Network's `dhcp:` mapping (docs/Decisions.md §31, #490): what
// IPAM hands out over DHCP, and the dynamic pool. Read the mode through
// Network.DHCPMode, which applies the default, and the pool through
// Network.Allocation, which derives it when it's omitted.
type DHCP struct {
	// Mode is the dhcp.mode as written: empty when omitted. It's kept raw,
	// as Instance.Runtime is, and the store keeps it raw too; configdiff
	// treats empty and DHCPOff as the same.
	Mode DHCPMode `yaml:"mode" json:",omitempty"`
	// Pool is the dynamic pool as written: the zero Range when omitted.
	Pool Range `yaml:"pool"`
}

// DHCPMode is a Network's dhcp.mode. It implements encoding.TextUnmarshaler,
// so an unknown mode is a parse error, like a malformed address.
type DHCPMode string

// The dhcp.mode values (docs/Decisions.md §31). What each makes of a
// Network's two ranges is Network.Allocation's.
const (
	// DHCPOff means IPAM hands out nothing; the router does DHCP.
	DHCPOff DHCPMode = "off"
	// DHCPProxy means IPAM hands out boot options only, beside the router.
	DHCPProxy DHCPMode = "proxy"
	// DHCPReservations means IPAM hands out its reservations, with no dynamic
	// pool, beside the router.
	DHCPReservations DHCPMode = "reservations"
	// DHCPFull means IPAM is the one DHCP authority: reservations plus dynamic
	// leases from dhcp.pool, which holds the nodes' addresses too.
	DHCPFull DHCPMode = "full"
)

// DefaultDHCPMode is the mode of a Network whose dhcp.mode is omitted:
// today's behaviour, with the router doing DHCP.
const DefaultDHCPMode = DHCPOff

// dhcpModes lists the dhcp.mode values, in the order messages name them.
var dhcpModes = []DHCPMode{DHCPOff, DHCPProxy, DHCPReservations, DHCPFull}

// UnmarshalText accepts one of the dhcp.mode values. An empty input yields
// the empty (omitted) mode with no error, as Range does.
func (m *DHCPMode) UnmarshalText(text []byte) error {
	s := DHCPMode(strings.TrimSpace(string(text)))
	if s != "" && !slices.Contains(dhcpModes, s) {
		names := make([]string, len(dhcpModes))
		for i, d := range dhcpModes {
			names[i] = string(d)
		}
		return fmt.Errorf("dhcp.mode %q: want one of %s", s, strings.Join(names, ", "))
	}
	*m = s
	return nil
}

// ReservesByMAC reports whether m hands out a reservation per Instance mac,
// which is what makes a mac required, and unique, on a network in that mode.
func (m DHCPMode) ReservesByMAC() bool { return m == DHCPReservations || m == DHCPFull }

// DHCPMode returns n's dhcp.mode, or DefaultDHCPMode when it's omitted.
func (n Network) DHCPMode() DHCPMode {
	if n.DHCP.Mode == "" {
		return DefaultDHCPMode
	}
	return n.DHCP.Mode
}

// WithAddressingOf returns n with its addressing fields (the ones
// AddressingFields names) taken from s, keeping n's name and dhcp.mode. It's
// how the web app's resolve step joins a git Network with its addressing-file
// entry, kept beside AddressingFields so the two lists can't drift.
func (n Network) WithAddressingOf(s Network) Network {
	n.CIDR = s.CIDR
	n.Gateway = s.Gateway
	n.DHCPExcludedRange = s.DHCPExcludedRange
	n.DNS = s.DNS
	n.DHCP.Pool = s.DHCP.Pool
	return n
}

// AddressingFields names the addressing fields n sets, in schema order:
// cidr, gateway, dhcp_excluded_range, dns, dhcp.pool. WithAddressingOf
// copies the same set.
func (n Network) AddressingFields() []string {
	var set []string
	if n.CIDR.IsValid() {
		set = append(set, "cidr")
	}
	if n.Gateway.IsValid() {
		set = append(set, "gateway")
	}
	if r := n.DHCPExcludedRange; r.Start.IsValid() || r.End.IsValid() {
		set = append(set, "dhcp_excluded_range")
	}
	if len(n.DNS) > 0 {
		set = append(set, "dns")
	}
	if r := n.DHCP.Pool; r.Start.IsValid() || r.End.IsValid() {
		set = append(set, "dhcp.pool")
	}
	return set
}

// Unaddressed reports whether n sets none of the addressing fields. In a
// synced fleet that makes it state-addressed (docs/Decisions.md §28).
func (n Network) Unaddressed() bool { return len(n.AddressingFields()) == 0 }

// Range is a closed inclusive address range written as "<start>-<end>" (used
// for a Network's dhcp_excluded_range and dhcp.pool). It implements encoding.TextMarshaler/
// TextUnmarshaler so it round-trips through YAML, JSON, and the store's TEXT
// columns. The zero Range (unset/omitted) has an invalid Start; detect it with
// !r.Start.IsValid().
type Range struct {
	Start netip.Addr
	End   netip.Addr
}

// UnmarshalText parses "<start>-<end>" into the Range. An empty input yields
// the zero Range with no error, so an omitted/blank dhcp_excluded_range is a
// valid "unset" rather than a parse failure.
func (r *Range) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*r = Range{}
		return nil
	}
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return fmt.Errorf("range %q: want \"<start>-<end>\"", s)
	}
	a, err := netip.ParseAddr(strings.TrimSpace(start))
	if err != nil {
		return fmt.Errorf("range %q: start: %w", s, err)
	}
	b, err := netip.ParseAddr(strings.TrimSpace(end))
	if err != nil {
		return fmt.Errorf("range %q: end: %w", s, err)
	}
	*r = Range{Start: a, End: b}
	return nil
}

// MarshalText renders the Range as "<start>-<end>", or the empty string for
// the zero Range (mirroring netip.Addr's empty-string encoding of its zero
// value, so the store's TEXT round-trip is lossless).
func (r Range) MarshalText() ([]byte, error) {
	if !r.Start.IsValid() {
		return []byte{}, nil
	}
	return []byte(r.Start.String() + "-" + r.End.String()), nil
}

// String renders the Range as MarshalText does.
func (r Range) String() string {
	b, _ := r.MarshalText()
	return string(b)
}

// Contains reports whether ip lies in the Range, ends included.
func (r Range) Contains(ip netip.Addr) bool {
	return r.Start.IsValid() && ip.Compare(r.Start) >= 0 && ip.Compare(r.End) <= 0
}

// Security holds an Instance's TPM/Secure Boot configuration.
type Security struct {
	TPM        bool `yaml:"tpm"`
	SecureBoot bool `yaml:"secure_boot"`
}

// ParseMAC parses s as a 48-bit Ethernet MAC, in any form net.ParseMAC
// accepts. net.ParseMAC also accepts 8-octet EUI-64 and 20-octet InfiniBand
// addresses, which no Ethernet NIC carries: a node's eth0 match or a DHCP
// reservation keyed on one would never match (#490).
func ParseMAC(s string) (net.HardwareAddr, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return nil, err
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("a %d-octet hardware address, not a 6-octet Ethernet MAC", len(hw))
	}
	return hw, nil
}

// Instance is a parsed `kind: Instance` document. StaticIP is a netip.Addr
// (zero/invalid when unset → DHCP); MAC stays a string, as written, because
// net.HardwareAddr has no stdlib TextUnmarshaler, but Parse checks it with
// ParseMAC.
type Instance struct {
	Name         string     `yaml:"name"`
	MAC          string     `yaml:"mac"`
	Network      string     `yaml:"network"`
	StaticIP     netip.Addr `yaml:"static_ip"`
	Disk         string     `yaml:"disk"`
	NIC          string     `yaml:"nic"`
	Security     Security   `yaml:"security"`
	Applications []string   `yaml:"applications"`
	// OS is the host operating system this machine runs, as written in the
	// fleet YAML: empty when omitted. Read it through HostOS, which applies
	// the DefaultOS default. The raw value is kept, not defaulted at parse,
	// so an omitted os and the store's round-trip (which has no os column
	// yet) compare equal in configdiff. omitempty keeps GET /instances
	// unchanged for the default.
	OS string `yaml:"os" json:",omitempty"`
	// Runtime is the cluster runtime this machine runs workloads under, as
	// written in the fleet YAML: empty when omitted. Read it through
	// ClusterRuntime, which applies the DefaultRuntime default. It lives on
	// the Instance because config has no cluster entity: every Instance in a
	// fleet is a member of its one cluster, so Validate requires them all to
	// resolve to the same runtime (docs/Config Schema.md § kind: Instance).
	// The raw value is kept, as for OS, and the store keeps it raw too, so
	// omitempty keeps GET /instances unchanged for the default.
	Runtime string `yaml:"runtime" json:",omitempty"`
	// TunnelIP is this instance's assigned address on the WireGuard overlay
	// (internal/wireguard.OverlayCIDR) — always app-assigned
	// (internal/wireguard.AssignTunnelIPs), never accepted from synced
	// fleet YAML, unlike StaticIP.
	TunnelIP netip.Addr `yaml:"-"`
}

// Host OS names an Instance's os field accepts (docs/Decisions.md §31). Every
// name here is known to the config format, but only those in
// implementedHostOSes have an internal/hostos backend; Validate rejects the
// rest with a message distinct from an unknown name's.
const (
	OSIncusOS = "incusos"
	OSTalos   = "talos"
	OSDebian  = "debian"
)

// DefaultOS is the host OS of an Instance whose os is omitted: every machine
// was IncusOS before the field existed.
const DefaultOS = OSIncusOS

// hostOSes lists the known host OS names, in the order messages name them.
var hostOSes = []string{OSIncusOS, OSTalos, OSDebian}

// implementedHostOSes are the host OSes with an internal/hostos backend.
// hostos's tests check this set against its registered backends, so the two
// can't drift: a backend lands together with its entry here.
var implementedHostOSes = map[string]bool{OSIncusOS: true}

// implementedHostOSList is implementedHostOSes in hostOSes order, for
// messages. It's derived once from the two above, so editing either keeps it
// in step.
var implementedHostOSList = func() []string {
	var out []string
	for _, name := range hostOSes {
		if implementedHostOSes[name] {
			out = append(out, name)
		}
	}
	return out
}()

// HostOSes returns every host OS name the config format knows, implemented
// or not.
func HostOSes() []string { return append([]string(nil), hostOSes...) }

// HostOSImplemented reports whether name has an internal/hostos backend.
func HostOSImplemented(name string) bool { return implementedHostOSes[name] }

// HostOS returns the instance's host OS: its os field, or DefaultOS when
// that's omitted.
func (i Instance) HostOS() string {
	if i.OS == "" {
		return DefaultOS
	}
	return i.OS
}

// Cluster runtime names an Instance's runtime field accepts
// (docs/Decisions.md §31). As with host OSes, every name here is known to the
// config format, but only those in implementedRuntimes have a backend;
// Validate rejects the rest with a message distinct from an unknown name's.
const (
	RuntimeIncus      = "incus"
	RuntimeKubernetes = "kubernetes"
)

// DefaultRuntime is the cluster runtime of an Instance whose runtime is
// omitted: every cluster was Incus before the field existed.
const DefaultRuntime = RuntimeIncus

// runtimes lists the known cluster runtime names, in the order messages name
// them.
var runtimes = []string{RuntimeIncus, RuntimeKubernetes}

// implementedRuntimes are the cluster runtimes this build can run.
var implementedRuntimes = map[string]bool{RuntimeIncus: true}

// implementedRuntimeList is implementedRuntimes in runtimes order, for
// messages.
var implementedRuntimeList = func() []string {
	var out []string
	for _, name := range runtimes {
		if implementedRuntimes[name] {
			out = append(out, name)
		}
	}
	return out
}()

// allowedRuntimes is the allowed-pairs table of host OS and cluster runtime
// (docs/Decisions.md §31): for each known host OS, the runtimes it can run,
// in runtimes order. It's the one place the pairing is encoded, so a new host
// OS or runtime adds or extends a row here; TestAllowedRuntimesCoversKnownNames
// fails if a known host OS has no row or a row names an unknown runtime.
var allowedRuntimes = map[string][]string{
	OSIncusOS: {RuntimeIncus},
	OSTalos:   {RuntimeKubernetes},
	OSDebian:  {RuntimeIncus, RuntimeKubernetes},
}

// Runtimes returns every cluster runtime name the config format knows,
// implemented or not.
func Runtimes() []string { return append([]string(nil), runtimes...) }

// RuntimeImplemented reports whether name is a cluster runtime this build can
// run.
func RuntimeImplemented(name string) bool { return implementedRuntimes[name] }

// ClusterRuntime returns the instance's cluster runtime: its runtime field,
// or DefaultRuntime when that's omitted.
func (i Instance) ClusterRuntime() string {
	if i.Runtime == "" {
		return DefaultRuntime
	}
	return i.Runtime
}

// Replicas is an App's cardinality: a fixed count, or one per known Instance
// ("per-node" — the DaemonSet shape, used by the agent itself and by Alloy).
// It implements encoding.TextUnmarshaler, exactly as Range does for a
// Network's dhcp_excluded_range, so "3" and "per-node" both parse and are
// syntactically validated at parse time (yaml.v3 honors TextUnmarshaler — see
// ExampleNetwork_yamlUnmarshal). The field is required: the zero Replicas is
// not a default meaning 1, it's an unset field Validate rejects.
//
// This is cardinality, NOT placement: it says how many, never where. A
// placement field (targeting an Incus cluster group) is anticipated as a
// separate field, invalid in combination with PerNode — see
// docs/Out of Scope.md. App.Node was deleted precisely because it meant both
// at once; Replicas must not inherit that conflation.
type Replicas struct {
	PerNode bool
	Count   int
}

// perNode is the one non-numeric replicas value.
const perNode = "per-node"

// UnmarshalText parses "per-node" or a decimal count. An empty input yields
// the zero Replicas with no error, mirroring Range: whether an unset field is
// acceptable is Validate's call, not the parser's. A count that is zero or
// negative parses here and is rejected by Validate too, exactly as Range
// parses a start-after-end range and leaves that semantics to Validate.
func (r *Replicas) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*r = Replicas{}
		return nil
	}
	if s == perNode {
		*r = Replicas{PerNode: true}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("replicas %q: want %q or a count", s, perNode)
	}
	*r = Replicas{Count: n}
	return nil
}

// MarshalText renders "per-node" or the count, and the empty string for the
// zero Replicas (mirroring Range's empty-string encoding of its zero value, so
// the store's TEXT round-trip is lossless).
func (r Replicas) MarshalText() ([]byte, error) {
	switch {
	case r.PerNode:
		return []byte(perNode), nil
	case r.Count == 0:
		return []byte{}, nil
	default:
		return []byte(strconv.Itoa(r.Count)), nil
	}
}

// ImageRef mirrors lxcapi.InstanceSource's remote-image fields closely enough
// to build one directly. Confirmed real-world shape: Protocol "oci" with
// Server "https://ghcr.io" pulls straight from GHCR (Incus's OCI remote
// support, shared/cliconfig/remote.go); the local dev/validation loop instead
// points Server at a docker-compose "registry" service.
type ImageRef struct {
	Server      string `yaml:"server,omitempty"`
	Protocol    string `yaml:"protocol,omitempty"` // "oci" | "simplestreams" | "incus" | ""
	Alias       string `yaml:"alias,omitempty"`
	Fingerprint string `yaml:"fingerprint,omitempty"`
}

// App is a parsed `kind: App` document: a workload instance the app-manager
// agent fleet reconciles against live Incus, via the renderer registered for
// Type. Distinct from Instance.Applications (IncusOS host-level applications,
// e.g. "incus" itself) — an App is a workload the agent manages, not a
// host-level IncusOS application. No placement field: Incus's own scheduler
// decides where an App runs in 0.x.
type App struct {
	Name     string            `yaml:"name"`     // unique across the fleet
	Type     string            `yaml:"type"`     // renderer-registry key
	Replicas Replicas          `yaml:"replicas"` // how many; required
	Image    ImageRef          `yaml:"image"`
	Params   map[string]string `yaml:"params,omitempty"` // opaque, renderer-specific passthrough
}

// Designation is a parsed `kind: Designation` document: which node's agent
// leads (internal/leaderelection.Designated, docs/Decisions.md §25). Primary
// names an Instance — a node, not an agent instance — so an agent's own
// blue-green self-upgrade needs no designation change. There is deliberately
// no epoch: since #212 a stale checkout is fenced by git commits, not by a
// counter the operator maintains.
//
// Parse collects every Designation it finds, like every other kind; "at most
// one per repo" is Validate's rule, not the parser's.
type Designation struct {
	Primary string `yaml:"primary"`
}

// Config holds every Network, Instance, App and Designation document parsed
// from a fleet definition file.
type Config struct {
	Networks     []Network
	Instances    []Instance
	Apps         []App
	Designations []Designation
}

// Append adds every document in o to c, preserving order. It's how a config
// repo's files, parsed one at a time, merge into one fleet — in one place, so
// a new kind can't be parsed and then silently dropped by a merge that
// forgot it.
func (c *Config) Append(o Config) {
	c.Networks = append(c.Networks, o.Networks...)
	c.Instances = append(c.Instances, o.Instances...)
	c.Apps = append(c.Apps, o.Apps...)
	c.Designations = append(c.Designations, o.Designations...)
}

// Primary returns the designated primary node, or "" when the repo declares
// no Designation (so no agent leads). It reads the first Designation; Validate
// rejects a repo with more than one, so callers should only ask a validated
// Config.
func (c Config) Primary() string {
	if len(c.Designations) == 0 {
		return ""
	}
	return c.Designations[0].Primary
}

type discriminator struct {
	Kind string `yaml:"kind"`
}

// Parse reads a multi-document, k8s-style YAML fleet definition and returns
// the parsed Networks, Instances, Apps and Designations. Each document must
// set `kind: Network`, `kind: Instance`, `kind: App` or `kind: Designation`;
// any other or missing kind is an error.
func Parse(r io.Reader) (Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(r)
	for i := 0; ; i++ {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if errors.Is(err, io.EOF) {
				return cfg, nil
			}
			return Config{}, fmt.Errorf("decode document %d: %w", i, err)
		}

		var d discriminator
		if err := node.Decode(&d); err != nil {
			return Config{}, fmt.Errorf("decode document %d: %w", i, err)
		}

		switch d.Kind {
		case "Network":
			// Inline the discriminator alongside Network so the required
			// kind field isn't itself flagged as unknown by the strict decode.
			var doc struct {
				Kind    string `yaml:"kind"`
				Network `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as Network: %w", i, err)
			}
			cfg.Networks = append(cfg.Networks, doc.Network)
		case "Instance":
			var doc struct {
				Kind     string `yaml:"kind"`
				Instance `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as Instance: %w", i, err)
			}
			// A mac is syntax-checked here, like an address field: a wrong
			// one leaves the node's network unconfigured at boot, and a DHCP
			// reservation keyed on it never matches (#490).
			if doc.MAC != "" {
				if _, err := ParseMAC(doc.MAC); err != nil {
					return Config{}, fmt.Errorf("decode document %d as Instance: mac %q: %w", i, doc.MAC, err)
				}
			}
			cfg.Instances = append(cfg.Instances, doc.Instance)
		case "App":
			var doc struct {
				Kind string `yaml:"kind"`
				App  `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as App: %w", i, err)
			}
			cfg.Apps = append(cfg.Apps, doc.App)
		case "Designation":
			var doc struct {
				Kind        string `yaml:"kind"`
				Designation `yaml:",inline"`
			}
			if err := strictDecode(&node, &doc); err != nil {
				return Config{}, fmt.Errorf("decode document %d as Designation: %w", i, err)
			}
			cfg.Designations = append(cfg.Designations, doc.Designation)
		case "":
			return Config{}, fmt.Errorf("document %d: missing required field %q", i, "kind")
		default:
			return Config{}, fmt.Errorf("document %d: unrecognized kind %q", i, d.Kind)
		}
	}
}

// strictDecode re-decodes an already-parsed document node into out with
// unknown-field detection on, so a typo'd or misplaced key (e.g. "statc_ip"
// silently becoming a DHCP fallback) is a parse error rather than a silently
// dropped field — these configs provision real hardware. yaml.Node.Decode
// can't do this itself: KnownFields is a yaml.Decoder option the node-decode
// path bypasses, so the node is round-tripped back through a strict Decoder.
func strictDecode(node *yaml.Node, out any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("re-encode document: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}
