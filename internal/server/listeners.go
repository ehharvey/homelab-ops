package server

import (
	"log"
	"net/http"
	"net/netip"

	"github.com/ehharvey/homelab-ops/internal/wireguard"
)

// OperatorsOnly wraps next so that it serves only requests whose source
// address belongs to an operator peer, and refuses every other request with
// 403 (#195, docs/Decisions.md §28). It is the whole of the tunnel listener's
// access control: no auth code beyond an address check.
//
// The check is sound only on the tunnel listener. There, a connection's
// source is an overlay address, and WireGuard's cryptokey routing drops any
// packet whose source isn't in the sending peer's allowed IPs, so the address
// identifies the key that sent it. Node peers, and anyone holding a node's
// leaked key, arrive from instance addresses and are refused. On a host
// listener the source address proves nothing, so this filter isn't applied
// there.
func OperatorsOnly(operators []wireguard.OperatorPeer, next http.Handler) http.Handler {
	allowed := make(map[netip.Addr]bool, len(operators))
	for _, op := range operators {
		allowed[op.Addr] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !allowed[src.Addr().Unmap()] {
			//nolint:gosec // G706: RemoteAddr comes from the tunnel's own netstack, and %q quotes the operator-visible values
			log.Printf("tunnel API: refused %s %q from %q: not an operator peer", r.Method, r.URL.Path, r.RemoteAddr)
			http.Error(w, "forbidden: not an operator peer", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewHealthHandler serves GET /healthz and nothing else. cmd/web runs it on
// its own local port for container health checks, whatever the API listener
// setting (#195), so a health probe never needs the API itself to be
// reachable.
func NewHealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}
