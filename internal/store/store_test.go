package store

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
)

func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s, ctx
}

func TestOpenCreatesSchema(t *testing.T) {
	s, ctx := openTestStore(t)

	_, _, ok, err := s.LastSync(ctx)
	if err != nil {
		t.Fatalf("LastSync: %v", err)
	}
	if ok {
		t.Fatalf("LastSync ok = true on a fresh store, want false")
	}
}

func sampleConfig() config.Config {
	return config.Config{
		Networks: []config.Network{
			{
				Name: "dev-lan", CIDR: netip.MustParsePrefix("10.0.0.0/24"), Gateway: netip.MustParseAddr("10.0.0.1"),
				DHCPExcludedRange: config.Range{Start: netip.MustParseAddr("10.0.0.1"), End: netip.MustParseAddr("10.0.0.10")},
				DNS:               []netip.Addr{netip.MustParseAddr("1.1.1.1")},
			},
		},
		Instances: []config.Instance{
			{
				Name: "devnode0", MAC: "aa:bb:cc:dd:ee:ff", Network: "dev-lan", StaticIP: netip.MustParseAddr("10.0.0.20"),
				Disk: "/dev/sda", NIC: "eth0",
				Security:     config.Security{TPM: true, SecureBoot: false},
				Applications: []string{"incus"},
			},
		},
		Apps: []config.App{
			{
				Name: "agent", Type: "agent", Replicas: config.Replicas{PerNode: true},
				Image:  config.ImageRef{Server: "https://ghcr.io", Protocol: "oci", Alias: "ehharvey/homelab-ops/agent:latest"},
				Params: map[string]string{"LOG_LEVEL": "debug"},
			},
			{
				Name: "web-frontend", Type: "some-renderer", Replicas: config.Replicas{Count: 3},
				Image: config.ImageRef{Fingerprint: "abc123"},
			},
		},
	}
}

func TestReplaceThenQuery(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := sampleConfig()
	now := time.Now()

	if err := s.Replace(ctx, cfg, "deadbeef", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	networks, err := s.Networks(ctx)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if len(networks) != 1 || !reflect.DeepEqual(networks[0], cfg.Networks[0]) {
		t.Errorf("Networks = %+v, want %+v", networks, cfg.Networks)
	}

	n, ok, err := s.Network(ctx, "dev-lan")
	if err != nil || !ok {
		t.Fatalf("Network(dev-lan) = %+v, %v, %v", n, ok, err)
	}
	if !reflect.DeepEqual(n, cfg.Networks[0]) {
		t.Errorf("Network(dev-lan) = %+v, want %+v", n, cfg.Networks[0])
	}

	instances, err := s.Instances(ctx)
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(instances) != 1 || instances[0].Name != "devnode0" {
		t.Errorf("Instances = %+v, want one devnode0", instances)
	}

	i, ok, err := s.Instance(ctx, "devnode0")
	if err != nil || !ok {
		t.Fatalf("Instance(devnode0) = %+v, %v, %v", i, ok, err)
	}
	if !reflect.DeepEqual(i, cfg.Instances[0]) {
		t.Errorf("Instance(devnode0) = %+v, want %+v", i, cfg.Instances[0])
	}

	// Ordered by name, so agent precedes web-frontend regardless of the order
	// they were declared in.
	apps, err := s.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if !reflect.DeepEqual(apps, cfg.Apps) {
		t.Errorf("Apps = %+v, want %+v", apps, cfg.Apps)
	}

	commit, syncedAt, ok, err := s.LastSync(ctx)
	if err != nil || !ok {
		t.Fatalf("LastSync = %q, %v, %v, %v", commit, syncedAt, ok, err)
	}
	if commit != "deadbeef" {
		t.Errorf("LastSync commit = %q, want deadbeef", commit)
	}
	if !syncedAt.Equal(now.UTC().Truncate(time.Second)) {
		t.Errorf("LastSync syncedAt = %v, want ~%v", syncedAt, now)
	}
}

// TestReplaceRoundTripsUnsetOptionals pins the zero-value round-trip the
// asText/UnmarshalText TEXT encoding promises: a DHCP network (no gateway, no
// dhcp_excluded_range, no DNS) and a DHCP instance (no static_ip) must read
// back byte-for-byte equal, with the unset address fields landing as the zero
// value (IsValid()==false) and not erroring on the empty TEXT column.
func TestReplaceRoundTripsUnsetOptionals(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := config.Config{
		Networks:  []config.Network{{Name: "dhcp-lan", CIDR: netip.MustParsePrefix("10.0.0.0/24")}},
		Instances: []config.Instance{{Name: "dhcpnode", MAC: "aa:bb:cc:dd:ee:01", Network: "dhcp-lan"}},
	}
	if err := s.Replace(ctx, cfg, "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	n, ok, err := s.Network(ctx, "dhcp-lan")
	if err != nil || !ok {
		t.Fatalf("Network(dhcp-lan) = %+v, %v, %v", n, ok, err)
	}
	if !reflect.DeepEqual(n, cfg.Networks[0]) {
		t.Errorf("Network round-trip = %+v, want %+v", n, cfg.Networks[0])
	}
	if n.Gateway.IsValid() || n.DHCPExcludedRange.Start.IsValid() {
		t.Errorf("unset gateway/range did not round-trip to the zero value: %+v", n)
	}

	i, ok, err := s.Instance(ctx, "dhcpnode")
	if err != nil || !ok {
		t.Fatalf("Instance(dhcpnode) = %+v, %v, %v", i, ok, err)
	}
	if !reflect.DeepEqual(i, cfg.Instances[0]) {
		t.Errorf("Instance round-trip = %+v, want %+v", i, cfg.Instances[0])
	}
	if i.StaticIP.IsValid() {
		t.Errorf("unset static_ip did not round-trip to the zero value: %+v", i)
	}
}

func TestReplaceOverwritesPriorSnapshot(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.Replace(ctx, sampleConfig(), "first", time.Now()); err != nil {
		t.Fatalf("first Replace: %v", err)
	}

	second := config.Config{
		Networks:  []config.Network{{Name: "other-lan", CIDR: netip.MustParsePrefix("10.1.0.0/24")}},
		Instances: []config.Instance{{Name: "othernode"}},
	}
	if err := s.Replace(ctx, second, "second", time.Now()); err != nil {
		t.Fatalf("second Replace: %v", err)
	}

	networks, err := s.Networks(ctx)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if len(networks) != 1 || networks[0].Name != "other-lan" {
		t.Errorf("Networks after second Replace = %+v, want only other-lan (replace, not merge)", networks)
	}

	if _, ok, _ := s.Network(ctx, "dev-lan"); ok {
		t.Errorf("dev-lan still present after a Replace that didn't include it")
	}
}

func TestLastSyncReflectsMostRecentReplace(t *testing.T) {
	s, ctx := openTestStore(t)

	if err := s.Replace(ctx, sampleConfig(), "first", time.Now()); err != nil {
		t.Fatalf("first Replace: %v", err)
	}
	later := time.Now().Add(time.Hour)
	if err := s.Replace(ctx, sampleConfig(), "second", later); err != nil {
		t.Fatalf("second Replace: %v", err)
	}

	commit, syncedAt, ok, err := s.LastSync(ctx)
	if err != nil || !ok {
		t.Fatalf("LastSync = %q, %v, %v, %v", commit, syncedAt, ok, err)
	}
	if commit != "second" {
		t.Errorf("LastSync commit = %q, want second", commit)
	}
	if !syncedAt.Equal(later.UTC().Truncate(time.Second)) {
		t.Errorf("LastSync syncedAt = %v, want ~%v", syncedAt, later)
	}
}

func TestNetworkNotFound(t *testing.T) {
	s, ctx := openTestStore(t)
	_, ok, err := s.Network(ctx, "nope")
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	if ok {
		t.Errorf("Network(nope) ok = true, want false")
	}
}

func TestInstanceNotFound(t *testing.T) {
	s, ctx := openTestStore(t)
	_, ok, err := s.Instance(ctx, "nope")
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if ok {
		t.Errorf("Instance(nope) ok = true, want false")
	}
}

func TestReplaceDuplicateNameLastWins(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := config.Config{
		Networks: []config.Network{
			{Name: "dup", CIDR: netip.MustParsePrefix("10.0.0.0/24")},
			{Name: "dup", CIDR: netip.MustParsePrefix("10.1.0.0/24")},
		},
	}

	if err := s.Replace(ctx, cfg, "sha", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	networks, err := s.Networks(ctx)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if len(networks) != 1 || networks[0].CIDR != netip.MustParsePrefix("10.1.0.0/24") {
		t.Errorf("Networks = %+v, want one dup network with the last CIDR", networks)
	}
}

// Replicas is the one App field with a non-trivial TEXT encoding, so pin every
// form through a real round-trip rather than trusting MarshalText's unit test:
// per-node, a count, and the zero value a hand-built Config could carry.
func TestReplaceRoundTripsReplicas(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := config.Config{
		Apps: []config.App{
			{Name: "a-per-node", Type: "agent", Replicas: config.Replicas{PerNode: true}},
			{Name: "b-count", Type: "some-renderer", Replicas: config.Replicas{Count: 3}},
			{Name: "c-zero", Type: "some-renderer", Replicas: config.Replicas{}},
		},
	}
	if err := s.Replace(ctx, cfg, "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	apps, err := s.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if !reflect.DeepEqual(apps, cfg.Apps) {
		t.Errorf("Apps = %+v, want %+v", apps, cfg.Apps)
	}
}

// A nil Params marshals to JSON "null" and reads back nil; an empty non-nil map
// reads back empty. reflect.DeepEqual distinguishes the two, so both have to
// survive — the same nil-vs-empty discipline instances.applications already has.
func TestReplaceRoundTripsNilAndEmptyParams(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := config.Config{
		Apps: []config.App{
			{Name: "a-nil", Type: "some-renderer", Replicas: config.Replicas{Count: 1}, Params: nil},
			{Name: "b-empty", Type: "some-renderer", Replicas: config.Replicas{Count: 1}, Params: map[string]string{}},
		},
	}
	if err := s.Replace(ctx, cfg, "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	apps, err := s.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if !reflect.DeepEqual(apps, cfg.Apps) {
		t.Errorf("Apps = %+v, want %+v", apps, cfg.Apps)
	}
}

// An App removed from git must leave no row behind: Replace is a full snapshot
// swap, not a merge.
func TestReplaceRemovesStaleApps(t *testing.T) {
	s, ctx := openTestStore(t)
	first := config.Config{Apps: []config.App{
		{Name: "old-app", Type: "some-renderer", Replicas: config.Replicas{Count: 1}},
	}}
	if err := s.Replace(ctx, first, "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	second := config.Config{Apps: []config.App{
		{Name: "new-app", Type: "some-renderer", Replicas: config.Replicas{Count: 1}},
	}}
	if err := s.Replace(ctx, second, "cafebabe", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	apps, err := s.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if !reflect.DeepEqual(apps, second.Apps) {
		t.Errorf("Apps = %+v, want only %+v", apps, second.Apps)
	}
}

// Replace never assumes Validate ran, so a duplicate name is last-one-wins and
// warn-only here — matching networks/instances, and configdiff.Diff's own
// map-build dedup.
func TestReplaceDuplicateAppNameLastWins(t *testing.T) {
	s, ctx := openTestStore(t)
	cfg := config.Config{Apps: []config.App{
		{Name: "dup", Type: "first", Replicas: config.Replicas{Count: 1}},
		{Name: "dup", Type: "second", Replicas: config.Replicas{Count: 1}},
	}}
	if err := s.Replace(ctx, cfg, "deadbeef", time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	apps, err := s.Apps(ctx)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if len(apps) != 1 || !reflect.DeepEqual(apps[0], cfg.Apps[1]) {
		t.Errorf("Apps = %+v, want only %+v (last duplicate wins)", apps, cfg.Apps[1])
	}
}
