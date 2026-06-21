// Package server provides the homelab-ops web app's HTTP server, decoupled
// from cmd/web's process wiring so it can be exercised directly in tests.
package server

import "net/http"

// New builds the web app's HTTP handler.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
