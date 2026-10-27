package incuslocal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	lxcapi "github.com/lxc/incus/v7/shared/api"
)

// MemberOnline is the Status of a cluster member that is up and reachable.
// Incus's other values are "Offline" and "Evacuated"
// (internal/server/db/node.go in lxc/incus).
const MemberOnline = "Online"

// Member is one cluster member as ListMembers sees it.
type Member struct {
	// Name is the member's name, the value instances report as their
	// Location and the one a create's target names. A joiner's
	// dns.hostname sets it (docs/Decisions.md §27).
	Name string
	// Status is Incus's own status string; see MemberOnline.
	Status string
}

// ListMembers returns every member of the cluster this daemon belongs to
// (GET /1.0/cluster/members). An unclustered daemon has no members to list
// and answers with an error; callers that may run on one check for that
// first (an instance Location of UnclusteredLocation).
func (c *Client) ListMembers(ctx context.Context) ([]Member, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/1.0/cluster/members?recursion=1", nil)
	if err != nil {
		return nil, fmt.Errorf("build member list request: %w", err)
	}

	envelope, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("list cluster members: %w", err)
	}

	var raw []lxcapi.ClusterMember
	if err := json.Unmarshal(envelope.Metadata, &raw); err != nil {
		return nil, fmt.Errorf("decode cluster members: %w", err)
	}

	out := make([]Member, 0, len(raw))
	for _, m := range raw {
		out = append(out, Member{Name: m.ServerName, Status: m.Status})
	}
	return out, nil
}
