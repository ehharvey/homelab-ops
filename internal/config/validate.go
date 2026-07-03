package config

import (
	"fmt"
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
// static IP falling inside its network's DHCP-excluded (static) range.
//
// It returns all issues found rather than stopping at the first, so an
// operator fixing a bad commit sees every problem at once.
func Validate(c Config) Issues {
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
			add(path+".cidr", "must be a valid CIDR")
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

	for i, inst := range c.Instances {
		path := fmt.Sprintf("instances[%d]", i)
		if !inst.StaticIP.IsValid() {
			continue // DHCP — nothing address-wise to validate here.
		}
		n, ok := byName[inst.Network]
		if !ok {
			add(path+".network", fmt.Sprintf("references unknown network %q", inst.Network))
			continue
		}
		if !n.CIDR.IsValid() {
			continue // already reported against the network above
		}
		if !n.CIDR.Contains(inst.StaticIP) {
			add(path+".static_ip", fmt.Sprintf("%s is not inside cidr %s", inst.StaticIP, n.CIDR))
		}
		if r := n.DHCPExcludedRange; r.Start.IsValid() && r.End.IsValid() &&
			(inst.StaticIP.Compare(r.Start) < 0 || inst.StaticIP.Compare(r.End) > 0) {
			add(path+".static_ip", fmt.Sprintf("%s is outside dhcp_excluded_range %s-%s", inst.StaticIP, r.Start, r.End))
		}
	}

	return issues
}
