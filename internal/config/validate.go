package config

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Issue is a single semantic-validation failure: Path locates the offending
// field (e.g. "networks[0].gateway") and Message says what's wrong. This is
// deliberately the shape a validation library like zog produces
// (issue.Path/issue.Message) — it's the seam that lets Validate be re-driven
// by such a library later without changing callers. See
// docs/Development Conventions.md § Config validation.
type Issue struct {
	Path    string
	Message string
}

// Issues is the (ordered) result of Validate. It satisfies error so a caller
// can wrap it (e.g. server's ErrValidate); an empty Issues means the config
// passed semantic validation.
type Issues []Issue

// Empty reports whether Validate found no problems.
func (is Issues) Empty() bool { return len(is) == 0 }

// Error renders every issue as "path: message", joined by "; ".
func (is Issues) Error() string {
	parts := make([]string, len(is))
	for i, x := range is {
		parts[i] = x.Path + ": " + x.Message
	}
	return strings.Join(parts, "; ")
}

// Validate runs the cross-field semantic checks that Parse deliberately does
// not (Parse is structural/syntactic only). Syntactic well-formedness of the
// address fields is already guaranteed by net/netip's TextUnmarshaler at parse
// time; Validate covers the relationships between fields: name non-empty, CIDR
// present, gateway/DNS/static-IP/DHCP-range membership in the CIDR, and a
// static IP falling inside its network's DHCP-excluded (static) range. Every
// Instance needs a unique hostname-label name, a declared network, an os
// with an internal/hostos backend (omitted means DefaultOS), and a runtime
// (omitted means DefaultRuntime) that its os can run and this build
// implements; all Instances must share one os and one runtime, because a
// fleet is one cluster. For an
// App it covers the required fields Parse can't enforce (yaml.v3 has no notion
// of a required key, so an omitted `replicas:` reaches here as a zero value)
// and at most one per-node App per renderer type. For a Designation, it
// checks there's at most one and that it names a declared Instance.
//
// It returns all issues found rather than stopping at the first, so an
// operator fixing a bad commit sees every problem at once.
//
// Validate wants every Network fully addressed: a local file, or a synced
// fleet after the web app's resolve step (internal/addressing.Resolve) has
// filled in its state-addressed networks. A synced fleet before that step is
// ValidateUnresolved's.
func Validate(c Config) Issues { return validate(c, false) }

// ValidateUnresolved validates a fleet as synced from git, before any
// state-addressed network has its addressing filled in (docs/Decisions.md
// §28). It is Validate plus AddressingIssues, except that a Network setting no
// addressing at all passes: its addressing lives in the web app's state. The
// agent's Clone uses it, because the agent never reads addressing (§27).
func ValidateUnresolved(c Config) Issues {
	return append(AddressingIssues(c), validate(c, true)...)
}

// AddressingIssues runs the git-side checks of §28's one-source rule on a
// synced fleet, naming the network in each message:
//   - a Network sets all its addressing (at least a cidr) or none of it, so a
//     gateway, dhcp_excluded_range or dns without a cidr is partial;
//   - a static_ip is valid in git only on a git-addressed network. A
//     state-addressed network's instances get IPAM addresses (pins for them,
//     held in state, are #514's).
func AddressingIssues(c Config) Issues {
	var issues Issues
	unaddressed := make(map[string]bool, len(c.Networks))
	for i, n := range c.Networks {
		set := n.AddressingFields()
		switch {
		case len(set) == 0:
			unaddressed[n.Name] = true
		case !n.CIDR.IsValid():
			issues = append(issues, Issue{
				Path: fmt.Sprintf("networks[%d]", i),
				Message: fmt.Sprintf("network %q sets %s but no cidr: a Network carries all its addressing or none of it (none takes it from the web app's addressing file)",
					n.Name, strings.Join(set, ", ")),
			})
		}
	}
	for i, inst := range c.Instances {
		if inst.StaticIP.IsValid() && unaddressed[inst.Network] {
			issues = append(issues, Issue{
				Path: fmt.Sprintf("instances[%d].static_ip", i),
				Message: fmt.Sprintf("network %q takes its addressing from the web app's state, so instance %q can't set a static_ip in git: omit it and IPAM assigns one",
					inst.Network, inst.Name),
			})
		}
	}
	return issues
}

// validate is Validate's body. With allowUnaddressed, a Network without a
// cidr skips the address checks, and so do its instances: AddressingIssues
// reports the partial ones and a static_ip on an unaddressed one.
func validate(c Config, allowUnaddressed bool) Issues {
	var issues Issues
	add := func(path, msg string) { issues = append(issues, Issue{Path: path, Message: msg}) }

	byName := make(map[string]Network, len(c.Networks))
	firstIndex := make(map[string]int, len(c.Networks))
	for i, n := range c.Networks {
		path := fmt.Sprintf("networks[%d]", i)
		byName[n.Name] = n

		if strings.TrimSpace(n.Name) == "" {
			add(path+".name", "must not be empty")
		} else if j, ok := firstIndex[n.Name]; ok {
			add(path+".name", fmt.Sprintf("%q is already defined by networks[%d]", n.Name, j))
		} else {
			firstIndex[n.Name] = i
		}
		if !n.CIDR.IsValid() {
			if !allowUnaddressed {
				add(path+".cidr", "must be a valid CIDR")
			}
			// Without a CIDR, the containment checks below are meaningless.
			continue
		}
		if n.Gateway.IsValid() && !n.CIDR.Contains(n.Gateway) {
			add(path+".gateway", fmt.Sprintf("%s is not inside cidr %s", n.Gateway, n.CIDR))
		}
		for j, d := range n.DNS {
			if !d.IsValid() {
				add(fmt.Sprintf("%s.dns[%d]", path, j), "must be a valid IP address")
			}
		}
		if r := n.DHCPExcludedRange; r.Start.IsValid() || r.End.IsValid() {
			rp := path + ".dhcp_excluded_range"
			switch {
			case !r.Start.IsValid() || !r.End.IsValid():
				add(rp, "must be a valid \"<start>-<end>\" range")
			case r.Start.Compare(r.End) > 0:
				add(rp, fmt.Sprintf("start %s is after end %s", r.Start, r.End))
			case !n.CIDR.Contains(r.Start) || !n.CIDR.Contains(r.End):
				add(rp, fmt.Sprintf("is not contained in cidr %s", n.CIDR))
			}
		}
	}

	instFirstIndex := make(map[string]int, len(c.Instances))
	for i, inst := range c.Instances {
		path := fmt.Sprintf("instances[%d]", i)

		// The name is the node's identity everywhere downstream: its Incus
		// cluster member name, its hostname, its store key, the agent's
		// AGENT_NODE_NAME, and a designation's primary. So it must be a name
		// every one of those accepts, not just non-empty.
		switch {
		case inst.Name == "":
			add(path+".name", "must not be empty")
		case !isHostnameLabel(inst.Name):
			add(path+".name", fmt.Sprintf("%q must be a lowercase hostname label: 1-63 of a-z, 0-9 and \"-\", not starting or ending with \"-\", and not all digits", inst.Name))
		default:
			if j, ok := instFirstIndex[inst.Name]; ok {
				add(path+".name", fmt.Sprintf("%q is already defined by instances[%d]", inst.Name, j))
			} else {
				instFirstIndex[inst.Name] = i
			}
		}

		validateHostOSAndRuntime(path, inst, add)

		// Checked for every instance, DHCP or not: an unknown network would
		// otherwise surface only later, as an IPAM or render error.
		n, ok := byName[inst.Network]
		if !ok {
			add(path+".network", fmt.Sprintf("references unknown network %q", inst.Network))
			continue
		}
		if !inst.StaticIP.IsValid() {
			continue // DHCP — nothing address-wise to validate here.
		}
		if !n.CIDR.IsValid() {
			continue // already reported against the network (or by AddressingIssues)
		}
		if !n.CIDR.Contains(inst.StaticIP) {
			add(path+".static_ip", fmt.Sprintf("%s is not inside cidr %s", inst.StaticIP, n.CIDR))
		}
		if r := n.DHCPExcludedRange; r.Start.IsValid() && r.End.IsValid() &&
			(inst.StaticIP.Compare(r.Start) < 0 || inst.StaticIP.Compare(r.End) > 0) {
			add(path+".static_ip", fmt.Sprintf("%s is outside dhcp_excluded_range %s-%s", inst.StaticIP, r.Start, r.End))
		}
		switch network, broadcast, ok := networkAndBroadcast(n.CIDR); {
		case n.Gateway.IsValid() && inst.StaticIP == n.Gateway:
			add(path+".static_ip", fmt.Sprintf("%s collides with the gateway", inst.StaticIP))
		case ok && inst.StaticIP == network:
			add(path+".static_ip", fmt.Sprintf("%s collides with the network address", inst.StaticIP))
		case ok && inst.StaticIP == broadcast:
			add(path+".static_ip", fmt.Sprintf("%s collides with the broadcast address", inst.StaticIP))
		}
	}

	appFirstIndex := make(map[string]int, len(c.Apps))
	perNodeFirstIndex := make(map[string]int, len(c.Apps))
	for i, a := range c.Apps {
		path := fmt.Sprintf("apps[%d]", i)

		if strings.TrimSpace(a.Name) == "" {
			add(path+".name", "must not be empty")
		} else if j, ok := appFirstIndex[a.Name]; ok {
			add(path+".name", fmt.Sprintf("%q is already defined by apps[%d]", a.Name, j))
		} else {
			appFirstIndex[a.Name] = i
		}
		// Type is required, but deliberately not checked against any renderer
		// registry: config is a leaf package with no knowledge of which
		// renderers a given binary registered. That check belongs at reconcile
		// time, skipped per-App rather than failing the whole sync.
		appType := strings.TrimSpace(a.Type)
		if appType == "" {
			add(path+".type", "must not be empty")
		}
		if strings.TrimSpace(a.Image.Alias) == "" && strings.TrimSpace(a.Image.Fingerprint) == "" {
			add(path+".image", "must set alias or fingerprint")
		}
		// replicas is required, so the zero value (an omitted field) is an
		// issue here alongside an explicit 0 or a negative — all three mean the
		// same thing to an operator, and one message covers them.
		if r := a.Replicas; !r.PerNode && r.Count < 1 {
			add(path+".replicas", fmt.Sprintf("must be %q or a positive count", perNode))
		} else if r.PerNode && appType != "" {
			// Keyed on the trimmed type, and skipped entirely when it's empty:
			// an App missing `type` is already reported above, and pairing two
			// of them off as duplicate per-node apps of type "" would report
			// one mistake twice — the same double-reporting the empty-name
			// branch avoids.
			if j, ok := perNodeFirstIndex[appType]; ok {
				add(path+".replicas", fmt.Sprintf("a per-node app of type %q is already defined by apps[%d]", appType, j))
			} else {
				perNodeFirstIndex[appType] = i
			}
		}
	}

	validateOneCluster(c.Instances, add)
	validateDesignations(c, add)

	return issues
}

// validateHostOSAndRuntime checks inst's os and runtime against the known
// names, the allowed-pairs table (allowedRuntimes) and the implemented
// backends, in that order, so an Instance gets the one error that names its
// fix. An unknown os or runtime is a typo, and checking its pairing would only
// add noise. A disallowed pair is reported alone, even when its os or runtime
// also has no backend, because adding that backend wouldn't make the pair
// valid. Only an allowed pair goes on to the no-backend errors, os then
// runtime. An unknown name is told apart from a known one without a backend
// because the operator's fix differs.
func validateHostOSAndRuntime(path string, inst Instance, add func(path, msg string)) {
	hostOS, rt := inst.HostOS(), inst.ClusterRuntime()
	switch allowed := allowedRuntimes[hostOS]; {
	case !slices.Contains(hostOSes, hostOS):
		add(path+".os", hostOSIssue(hostOS))
	case !slices.Contains(runtimes, rt):
		add(path+".runtime", runtimeIssue(rt))
	case !slices.Contains(allowed, rt):
		msg := fmt.Sprintf("os %q only runs %s, not %q", hostOS, strings.Join(allowed, " or "), rt)
		if inst.Runtime == "" {
			msg += " (the default when runtime is omitted)"
		}
		add(path+".runtime", msg)
	default:
		if msg := hostOSIssue(hostOS); msg != "" {
			add(path+".os", msg)
		}
		if msg := runtimeIssue(rt); msg != "" {
			add(path+".runtime", msg)
		}
	}
}

// validateOneCluster checks that every Instance resolves to the same host OS
// and the same cluster runtime. Config has no cluster entity, so every
// Instance in a fleet is a member of its one cluster, and a cluster's members
// all run one host OS (docs/Decisions.md §30) and one runtime. Resolved values
// are compared, so an omitted os or runtime equals its default. An unknown
// name is left out of the comparison: it already has its own error, and fixing
// that typo is the change the operator needs. If a repo can later hold several
// clusters (#181, #440), this narrows to each cluster's members.
func validateOneCluster(instances []Instance, add func(path, msg string)) {
	sameAcross := func(field string, value func(Instance) string, known []string, what string) {
		first, firstIdx := "", -1
		for i, inst := range instances {
			v := value(inst)
			if !slices.Contains(known, v) {
				continue
			}
			if firstIdx < 0 {
				first, firstIdx = v, i
				continue
			}
			if v != first {
				add(fmt.Sprintf("instances[%d].%s", i, field), fmt.Sprintf("%q differs from instances[%d]'s %q: a fleet is one cluster, and its members all run one %s", v, firstIdx, first, what))
			}
		}
	}
	sameAcross("os", Instance.HostOS, hostOSes, "host OS")
	sameAcross("runtime", Instance.ClusterRuntime, runtimes, "runtime")
}

// runtimeIssue returns why rt can't be used as an Instance's cluster runtime,
// or "" when it can.
func runtimeIssue(rt string) string {
	if RuntimeImplemented(rt) {
		return ""
	}
	if slices.Contains(runtimes, rt) {
		return fmt.Sprintf("%q has no backend yet; implemented: %s", rt, strings.Join(implementedRuntimeList, ", "))
	}
	return fmt.Sprintf("%q is not a known runtime; known: %s", rt, strings.Join(runtimes, ", "))
}

// hostOSIssue returns why hostOS can't be used as an Instance's host OS, or
// "" when it can.
func hostOSIssue(hostOS string) string {
	if HostOSImplemented(hostOS) {
		return ""
	}
	if slices.Contains(hostOSes, hostOS) {
		return fmt.Sprintf("%q has no backend yet; implemented: %s", hostOS, strings.Join(implementedHostOSList, ", "))
	}
	return fmt.Sprintf("%q is not a known host OS; known: %s", hostOS, strings.Join(hostOSes, ", "))
}

// isHostnameLabel reports whether s is a lowercase RFC 1123 hostname label:
// 1-63 characters of a-z, 0-9 and '-', neither starting nor ending with '-',
// and not all digits. Lowercase-only because hostnames are case-insensitive:
// "Node0" and "node0" would pass a uniqueness check yet name one host. It is
// stricter than Incus's own member-name check (validate.IsAPIName) and matches
// its IsHostname, so a name that passes is valid everywhere an Instance name
// goes.
func isHostnameLabel(s string) bool {
	if len(s) < 1 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	allDigits := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z', c == '-':
			allDigits = false
		default:
			return false
		}
	}
	return !allDigits
}

// validateDesignations checks the leader designation (#101): at most one per
// repo, and its primary must name a declared Instance. Zero is valid and means
// no agent leads — the safe default, matching MayAct's "unknown ⇒ not leader".
//
// Instance names are matched exactly as written. Whether those names are
// themselves well-formed is the Instance checks' concern, not this one's.
func validateDesignations(c Config, add func(path, msg string)) {
	instances := make(map[string]bool, len(c.Instances))
	for _, inst := range c.Instances {
		instances[inst.Name] = true
	}
	for i, d := range c.Designations {
		path := fmt.Sprintf("designations[%d]", i)
		if i > 0 {
			add(path, "at most one Designation per repo; already defined by designations[0]")
			continue
		}
		switch {
		case strings.TrimSpace(d.Primary) == "":
			add(path+".primary", "must name an Instance")
		case !instances[d.Primary]:
			add(path+".primary", fmt.Sprintf("references unknown instance %q", d.Primary))
		}
	}
}

// networkAndBroadcast returns p's network (lowest) and broadcast (highest)
// addresses. IPv4-only, mirroring ipam.NetworkPool's reserved-address
// exclusions; ok is false for a non-IPv4 prefix (p is already known-valid by
// the time callers reach this).
func networkAndBroadcast(p netip.Prefix) (network, broadcast netip.Addr, ok bool) {
	if !p.Addr().Is4() {
		return netip.Addr{}, netip.Addr{}, false
	}
	m := p.Masked()
	a := m.Addr().As4()
	v := binary.BigEndian.Uint32(a[:])
	v |= uint32(0xffffffff) >> m.Bits()
	binary.BigEndian.PutUint32(a[:], v)
	return m.Addr(), netip.AddrFrom4(a), true
}
