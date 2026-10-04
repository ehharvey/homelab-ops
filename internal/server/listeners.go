package server

import (
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"sync"
	"time"

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
	refusals := newRefusalLog(time.Now, refusalLogInterval)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !allowed[src.Addr().Unmap()] {
			if suppressed, ok := refusals.allow(src.Addr().Unmap()); ok {
				var more string
				if suppressed > 0 {
					more = fmt.Sprintf(" (and %d more from this address since the last such line)", suppressed)
				}
				//nolint:gosec // G706: RemoteAddr comes from the tunnel's own netstack, and %q quotes the operator-visible values
				log.Printf("tunnel API: refused %s %q from %q: not an operator peer%s", r.Method, r.URL.Path, r.RemoteAddr, more)
			}
			http.Error(w, "forbidden: not an operator peer", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refusalLogInterval is how often OperatorsOnly logs a refusal per source
// address, after logging the first one.
const refusalLogInterval = time.Minute

// refusalLog rate-limits OperatorsOnly's refusal lines per source address, so
// a peer that holds a node's key and sends requests in a loop can't grow the
// log without bound. The map stays small: the tunnel listener's sources are
// overlay addresses, which are at most OverlayCIDR's 256.
type refusalLog struct {
	now      func() time.Time
	interval time.Duration

	mu   sync.Mutex
	seen map[netip.Addr]*refusalCount
}

type refusalCount struct {
	logged     time.Time
	suppressed int
}

func newRefusalLog(now func() time.Time, interval time.Duration) *refusalLog {
	return &refusalLog{now: now, interval: interval, seen: make(map[netip.Addr]*refusalCount)}
}

// allow reports whether to log a refusal from src: always for its first one,
// then at most once per interval. It also reports how many refusals from src
// went unlogged since the last line that was logged.
func (l *refusalLog) allow(src netip.Addr) (suppressed int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c, seen := l.seen[src]
	if !seen {
		l.seen[src] = &refusalCount{logged: now}
		return 0, true
	}
	if now.Sub(c.logged) < l.interval {
		c.suppressed++
		return 0, false
	}
	suppressed = c.suppressed
	c.logged, c.suppressed = now, 0
	return suppressed, true
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
