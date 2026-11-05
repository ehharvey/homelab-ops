package config

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Allocation is how a Network's addresses divide between its static range
// (dhcp_excluded_range) and its DHCP pool (dhcp.pool), per its dhcp.mode
// (docs/Decisions.md §31, docs/Ipam.md, #490). Network.Allocation computes
// it, and config.Validate, internal/ipam and `bootstrap parse-config` all
// read it, so they can't disagree about which addresses a node may take.
type Allocation struct {
	Mode DHCPMode
	// Static is the static range: addresses the router (or, in full, IPAM)
	// never leases.
	Static Span
	// Pool is the DHCP pool: the router's dynamic pool, or in full IPAM's,
	// which holds the nodes' reservations too.
	Pool Span
	// Carved are the addresses inside the CIDR that are never allocated: the
	// network and broadcast addresses (IPv4), the gateway, and each in-CIDR
	// dns address, ascending. Each Span's Effective leaves them out.
	Carved []netip.Addr
}

// Nodes is the Span node addresses come from: the pool in full mode, the
// static range in every other.
func (a Allocation) Nodes() Span {
	if a.Mode == DHCPFull {
		return a.Pool
	}
	return a.Static
}

// IsCarved reports whether ip is one of a.Carved, never allocated.
func (a Allocation) IsCarved(ip netip.Addr) bool {
	_, found := slices.BinarySearchFunc(a.Carved, ip, netip.Addr.Compare)
	return found
}

// Span is one of a Network's two ranges, written or derived, uncarved and
// effective.
type Span struct {
	// Field is the YAML field the span is: "dhcp_excluded_range" or
	// "dhcp.pool".
	Field  string
	Source SpanSource
	// Uncarved is the range as written, or as derived: a derived complement
	// can be two disjoint spans, below and above the other field. Empty for
	// SpanUnset. An explicit static_ip is checked against it.
	Uncarved []Range
	// Effective is Uncarved minus the Allocation's Carved addresses, so
	// carving can split it further. Auto-assignment draws from it.
	Effective []Range
}

// SpanSource says where a Span came from.
type SpanSource int

const (
	// SpanUnset means neither field is written and the mode isn't full, so there
	// is no such range (and no auto-assign pool), as before #490.
	SpanUnset SpanSource = iota
	// SpanWritten means the field is written.
	SpanWritten
	// SpanComplement means the field is omitted and the other one is written, so
	// it's the other's complement within the CIDR.
	SpanComplement
	// SpanWholeCIDR means that in full mode, with neither field written, the pool is
	// the whole CIDR.
	SpanWholeCIDR
)

// Written reports whether the span's field is written, rather than derived
// or unset.
func (s Span) Written() bool { return s.Source == SpanWritten }

// Carved reports whether carving removed any address from the span.
func (s Span) Carved() bool { return !slices.Equal(s.Uncarved, s.Effective) }

// ContainsUncarved reports whether ip lies in the uncarved span.
func (s Span) ContainsUncarved(ip netip.Addr) bool {
	return slices.ContainsFunc(s.Uncarved, func(r Range) bool { return r.Contains(ip) })
}

// Describe names the span for a message: the field and its uncarved ranges,
// and, when it's derived, how.
func (s Span) Describe() string {
	switch s.Source {
	case SpanWritten:
		return s.Field + " " + FormatRanges(s.Uncarved)
	case SpanComplement:
		return fmt.Sprintf("%s %s (derived: the cidr minus %s)", s.Field, FormatRanges(s.Uncarved), otherField(s.Field))
	case SpanWholeCIDR:
		return fmt.Sprintf("%s %s (derived: the whole cidr)", s.Field, FormatRanges(s.Uncarved))
	default:
		return s.Field + " (unset)"
	}
}

// FormatRanges renders rs as "a-b, c-d", or "none" when it's empty.
func FormatRanges(rs []Range) string {
	if len(rs) == 0 {
		return "none"
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

const (
	staticField = "dhcp_excluded_range"
	poolField   = "dhcp.pool"
)

func otherField(f string) string {
	if f == staticField {
		return poolField
	}
	return staticField
}

// Allocation derives and carves n's static range and pool:
//   - either field, both or neither may be written; when only one is, the
//     other is its complement within the CIDR, which can be two spans;
//   - in full mode with neither written, the pool is the whole CIDR, and
//     there's no static range; in any other mode, neither written means
//     neither range (and no auto-assign pool);
//   - each Span's Effective is its Uncarved minus Carved.
//
// The issues are the written fields' own faults, with paths relative to the
// network ("cidr", "dhcp_excluded_range", "dhcp.pool"): a missing cidr, a
// malformed or reversed range, one outside the CIDR, or the two overlapping.
// With any issue the Allocation holds only Mode.
func (n Network) Allocation() (Allocation, Issues) {
	a := Allocation{Mode: n.DHCPMode()}
	if !n.CIDR.IsValid() {
		return a, Issues{{Path: "cidr", Message: "must be a valid CIDR"}}
	}
	var issues Issues
	static, staticSet := writtenRange(n.DHCPExcludedRange, staticField, n.CIDR, &issues)
	pool, poolSet := writtenRange(n.DHCP.Pool, poolField, n.CIDR, &issues)
	if !issues.Empty() {
		return a, issues
	}
	if staticSet && poolSet && static.Start.Compare(pool.End) <= 0 && pool.Start.Compare(static.End) <= 0 {
		return a, Issues{{Path: poolField, Message: fmt.Sprintf("%s overlaps dhcp_excluded_range %s: the static range and the pool must not share an address", pool, static)}}
	}

	prefix := n.CIDR.Masked()
	whole := Range{Start: prefix.Addr(), End: LastAddr(prefix)}
	a.Static = Span{Field: staticField}
	a.Pool = Span{Field: poolField}
	switch {
	case staticSet && poolSet:
		a.Static.Source, a.Static.Uncarved = SpanWritten, []Range{static}
		a.Pool.Source, a.Pool.Uncarved = SpanWritten, []Range{pool}
	case staticSet:
		a.Static.Source, a.Static.Uncarved = SpanWritten, []Range{static}
		a.Pool.Source, a.Pool.Uncarved = SpanComplement, complement(whole, static)
	case poolSet:
		a.Pool.Source, a.Pool.Uncarved = SpanWritten, []Range{pool}
		a.Static.Source, a.Static.Uncarved = SpanComplement, complement(whole, pool)
	case a.Mode == DHCPFull:
		a.Pool.Source, a.Pool.Uncarved = SpanWholeCIDR, []Range{whole}
	}

	a.Carved = carvedAddrs(n, prefix)
	a.Static.Effective = carve(a.Static.Uncarved, a.Carved)
	a.Pool.Effective = carve(a.Pool.Uncarved, a.Carved)
	return a, nil
}

// writtenRange checks a written range field against cidr, adding its issues
// to issues. It reports whether the field is written and sound.
func writtenRange(r Range, field string, cidr netip.Prefix, issues *Issues) (Range, bool) {
	if !r.Start.IsValid() && !r.End.IsValid() {
		return Range{}, false
	}
	add := func(msg string) { *issues = append(*issues, Issue{Path: field, Message: msg}) }
	switch {
	case !r.Start.IsValid() || !r.End.IsValid():
		add("must be a valid \"<start>-<end>\" range")
	case r.Start.Compare(r.End) > 0:
		add(fmt.Sprintf("start %s is after end %s", r.Start, r.End))
	case !cidr.Contains(r.Start) || !cidr.Contains(r.End):
		add(fmt.Sprintf("is not contained in cidr %s", cidr))
	default:
		return r, true
	}
	return Range{}, false
}

// complement returns whole minus r, which lies inside it: up to two spans,
// below and above r.
func complement(whole, r Range) []Range {
	out := []Range{}
	if r.Start.Compare(whole.Start) > 0 {
		out = append(out, Range{Start: whole.Start, End: r.Start.Prev()})
	}
	if r.End.Compare(whole.End) < 0 {
		out = append(out, Range{Start: r.End.Next(), End: whole.End})
	}
	return out
}

// carvedAddrs returns the addresses of n's CIDR that are never allocated,
// ascending and without duplicates: the network and broadcast addresses
// (IPv4 only, as IPAM is), the gateway, and each dns address inside the
// CIDR.
func carvedAddrs(n Network, prefix netip.Prefix) []netip.Addr {
	var out []netip.Addr
	if network, broadcast, ok := networkAndBroadcast(prefix); ok {
		out = append(out, network, broadcast)
	}
	if n.Gateway.IsValid() && prefix.Contains(n.Gateway) {
		out = append(out, n.Gateway)
	}
	for _, d := range n.DNS {
		if d.IsValid() && prefix.Contains(d) {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}

// carve returns rs minus holes (ascending), splitting a range around each
// hole inside it and dropping one that's left empty.
func carve(rs []Range, holes []netip.Addr) []Range {
	out := []Range{}
	for _, r := range rs {
		start, open := r.Start, true
		for _, h := range holes {
			if h.Compare(start) < 0 || h.Compare(r.End) > 0 {
				continue
			}
			if h != start {
				out = append(out, Range{Start: start, End: h.Prev()})
			}
			if h == r.End {
				open = false
				break
			}
			start = h.Next()
		}
		if open {
			out = append(out, Range{Start: start, End: r.End})
		}
	}
	return out
}

// LastAddr returns p's highest address, every host bit set: its broadcast
// address for IPv4. It takes any family, because Validate reaches Allocation
// before anything rejects an IPv6 cidr.
func LastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
