package apiclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DialFunc dials a TCP address, the signature of http.Transport.DialContext
// and of wireguard.Tunnel.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// HTTPClient returns a plain-HTTP client that dials every connection with
// dial, e.g. through the operator's WireGuard tunnel. It sets no overall
// timeout: an image download may run for minutes, and the caller's context
// bounds a request instead. #397 replaces this with a TLS client that pins
// the web app's cert, leaving Client unchanged.
func HTTPClient(dial DialFunc) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: dial,
		// One operator, one request per CLI run: nothing to pool.
		DisableKeepAlives: true,
		// No system proxy: the tunnel address is reachable only through dial.
		Proxy: nil,
	}}
}

// RetryDial wraps dial so a failed or unanswered dial is retried every
// interval until timeout has passed since the first attempt. Through a
// WireGuard tunnel the first dial also waits out the handshake, and a peer
// the far side doesn't know (a key not listed there) never answers at all,
// so timeout is how long to wait before calling the tunnel down. timeout
// bounds connecting only; a connected request runs under its own context.
func RetryDial(dial DialFunc, timeout, interval time.Duration) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var lastErr error
		for {
			conn, err := dial(dialCtx, network, address)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			select {
			case <-dialCtx.Done():
				return nil, fmt.Errorf("no answer from %s within %s; check the peer's public key and endpoint, and that this key is listed at this local address on the far side: %w", address, timeout, lastErr)
			case <-time.After(interval):
			}
		}
	}
}
