package wireguard

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/ehharvey/homelab-ops/internal/config"
)

func mustPub(t *testing.T) PublicKey {
	t.Helper()
	_, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	return pub
}

func TestParseOperatorPeersAcceptsCommaAndWhitespaceSeparatedEntries(t *testing.T) {
	a, b := mustPub(t), mustPub(t)
	raw := fmt.Sprintf(" 10.100.0.240=%s,\n10.100.0.254=%s ", a, b)

	peers, err := ParseOperatorPeers(raw)
	if err != nil {
		t.Fatalf("ParseOperatorPeers: %v", err)
	}
	want := []OperatorPeer{
		{PublicKey: a, Addr: netip.MustParseAddr("10.100.0.240")},
		{PublicKey: b, Addr: netip.MustParseAddr("10.100.0.254")},
	}
	if len(peers) != len(want) {
		t.Fatalf("got %d peers, want %d", len(peers), len(want))
	}
	for i := range want {
		if peers[i] != want[i] {
			t.Errorf("peers[%d] = %+v, want %+v", i, peers[i], want[i])
		}
	}
}

func TestParseOperatorPeersEmptyIsNoPeers(t *testing.T) {
	peers, err := ParseOperatorPeers("  ")
	if err != nil || len(peers) != 0 {
		t.Fatalf("ParseOperatorPeers(blank) = %v, %v; want no peers, nil", peers, err)
	}
}

func TestParseOperatorPeersRejects(t *testing.T) {
	a, b := mustPub(t), mustPub(t)
	cases := map[string]struct{ raw, wantErr string }{
		"no separator":           {raw: "10.100.0.240", wantErr: "want <overlay-address>=<public-key>"},
		"bad address":            {raw: "nope=" + a.String(), wantErr: "address"},
		"instance range address": {raw: "10.100.0.2=" + a.String(), wantErr: "outside the operator range"},
		"web app address":        {raw: "10.100.0.1=" + a.String(), wantErr: "outside the operator range"},
		"broadcast address":      {raw: "10.100.0.255=" + a.String(), wantErr: "outside the operator range"},
		"bad key":                {raw: "10.100.0.240=not-base64!", wantErr: "decode public key"},
		"short key":              {raw: "10.100.0.240=AAAA", wantErr: "want 32"},
		"duplicate address":      {raw: fmt.Sprintf("10.100.0.240=%s,10.100.0.240=%s", a, b), wantErr: "listed twice"},
		"duplicate key":          {raw: fmt.Sprintf("10.100.0.240=%s,10.100.0.241=%s", a, a), wantErr: "listed twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseOperatorPeers(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseOperatorPeers(%q) error = %v, want one containing %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}

// TestAssignTunnelIPsNeverUsesOperatorRange fills the whole instance pool and
// checks no instance lands in OperatorCIDR: an instance there would take the
// operator's address in the web app's peer table (#195).
func TestAssignTunnelIPsNeverUsesOperatorRange(t *testing.T) {
	// .2 to .239: OverlayCIDR minus network, WebAppAddr and OperatorCIDR.
	const pool = 238
	instances := make([]config.Instance, pool)
	for i := range instances {
		instances[i].Name = fmt.Sprintf("n%d", i)
	}
	if err := AssignTunnelIPs(instances, nil); err != nil {
		t.Fatalf("AssignTunnelIPs(%d instances): %v", pool, err)
	}
	for _, inst := range instances {
		if OperatorCIDR.Contains(inst.TunnelIP) {
			t.Fatalf("instance %q was assigned %s, inside the operator range %s", inst.Name, inst.TunnelIP, OperatorCIDR)
		}
	}

	more := make([]config.Instance, pool+1)
	for i := range more {
		more[i].Name = fmt.Sprintf("n%d", i)
	}
	if err := AssignTunnelIPs(more, instances); err == nil {
		t.Fatal("expected exhaustion once the instance pool is full, not a spill into the operator range")
	}
}

func TestAssignTunnelIPsDoesNotReusePriorAddressInOperatorRange(t *testing.T) {
	prior := []config.Instance{{Name: "a", TunnelIP: netip.MustParseAddr("10.100.0.240")}}
	instances := []config.Instance{{Name: "a"}}
	if err := AssignTunnelIPs(instances, prior); err != nil {
		t.Fatalf("AssignTunnelIPs: %v", err)
	}
	if OperatorCIDR.Contains(instances[0].TunnelIP) {
		t.Errorf("tunnel_ip %s reused from the operator range", instances[0].TunnelIP)
	}
}
