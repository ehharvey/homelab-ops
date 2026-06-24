// Package server provides the homelab-ops web app's HTTP server, decoupled
// from cmd/web's process wiring so it can be exercised directly in tests.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/configdiff"
	"github.com/ehharvey/homelab-ops/internal/ipam"
)

// Syncer pulls fleet config from its source and reports the resulting
// commit SHA. Implemented by internal/configsync.Syncer.
type Syncer interface {
	Sync(ctx context.Context) (config.Config, string, error)
}

// Store persists the synced Config snapshot so it survives across sync
// runs. Implemented by internal/store.Store.
type Store interface {
	Replace(ctx context.Context, cfg config.Config, commit string, now time.Time) error
	LastSync(ctx context.Context) (commit string, syncedAt time.Time, ok bool, err error)
	Networks(ctx context.Context) ([]config.Network, error)
	Instances(ctx context.Context) ([]config.Instance, error)
	Network(ctx context.Context, name string) (config.Network, bool, error)
	Instance(ctx context.Context, name string) (config.Instance, bool, error)
}

// CertSource provides the deployment's single break-glass client
// certificate to preseed into every rendered instance's incus.yaml. It is
// parameterless because the cert is one per cluster, not one per instance
// — see docs/Open Questions.md §4's follow-up.
type CertSource interface {
	ClientCertPEM(ctx context.Context) ([]byte, error)
}

// Service coordinates config syncs from one syncer into one store. It exists
// so a manual POST /sync and cmd/web's background poller share a single
// serialization point: the mutex below means their read-prior-then-replace
// sequences can't interleave (which would mis-attribute a diff warning) and
// two clones never run at once. syncer and store may be nil — see New.
type Service struct {
	syncer Syncer
	store  Store
	certs  CertSource
	mu     sync.Mutex
}

// NewService builds a Service over syncer and store, either of which may be
// nil (POST /sync and the read endpoints then report themselves unconfigured).
// certs may also be nil (the seed route then reports itself unconfigured).
func NewService(syncer Syncer, store Store, certs CertSource) *Service {
	return &Service{syncer: syncer, store: store, certs: certs}
}

// Sync runs one serialized sync cycle at time now; see SyncOnce for the
// per-stage semantics and error classification.
func (s *Service) Sync(ctx context.Context, now time.Time) (SyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SyncOnce(ctx, s.syncer, s.store, now)
}

// New builds the web app's HTTP handler. syncer, store, and certs may be
// nil, in which case POST /sync, the read endpoints, and the seed route
// report themselves as unconfigured rather than failing /healthz.
func New(syncer Syncer, store Store, certs CertSource) http.Handler {
	return NewFromService(NewService(syncer, store, certs))
}

// NewFromService builds the handler around an existing Service, so a caller
// (cmd/web) can share one sync serialization point between the HTTP handler
// and a background poller.
func NewFromService(svc *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("POST /sync", handleSync(svc))
	mux.HandleFunc("GET /status", handleStatus(svc.store))
	mux.HandleFunc("GET /networks", handleNetworks(svc.store))
	mux.HandleFunc("GET /instances", handleInstances(svc.store))
	mux.HandleFunc("POST /instances/{name}/seed", handleInstanceSeed(svc.store, svc.certs))
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type syncResponse struct {
	Commit    string     `json:"commit"`
	Networks  int        `json:"networks"`
	Instances int        `json:"instances"`
	Diff      diffCounts `json:"diff"`
}

// diffCounts summarizes a configdiff.Result for the sync response — counts
// only, not the full human-readable warning text (see Lines, which is
// log-only). The issue's done-when criterion ("visible diff/warning") is
// satisfied by the server log; the JSON response only needs to tell a
// caller that something changed, without committing the API to a
// long-form string contract this'll likely want to redesign once a UI
// exists.
type diffCounts struct {
	NetworksAdded    int `json:"networks_added"`
	NetworksChanged  int `json:"networks_changed"`
	NetworksRemoved  int `json:"networks_removed"`
	InstancesAdded   int `json:"instances_added"`
	InstancesChanged int `json:"instances_changed"`
	InstancesRemoved int `json:"instances_removed"`
}

func newDiffCounts(d configdiff.Result) diffCounts {
	return diffCounts{
		NetworksAdded:    len(d.AddedNetworks),
		NetworksChanged:  len(d.ChangedNetworks),
		NetworksRemoved:  len(d.RemovedNetworks),
		InstancesAdded:   len(d.AddedInstances),
		InstancesChanged: len(d.ChangedInstances),
		InstancesRemoved: len(d.RemovedInstances),
	}
}

// ErrSync, ErrValidate, ErrIPAM, and ErrStore classify a SyncOnce failure by
// stage: a pull/parse failure, a semantic-validation failure, or an
// IP-assignment data problem (all three upstream's fault — bad repo content —
// map to 502) versus a persistence failure (ours, maps to 500). Callers use
// errors.Is to tell them apart.
var (
	ErrSync     = errors.New("sync failed")
	ErrValidate = errors.New("config validation failed")
	ErrIPAM     = errors.New("ip assignment failed")
	ErrStore    = errors.New("store failed")
)

// SyncResult is the outcome of one successful SyncOnce: the synced commit,
// the parsed Config, and its diff against the prior stored snapshot.
type SyncResult struct {
	Commit string
	Config config.Config
	Diff   configdiff.Result
}

// SyncOnce runs one config-sync cycle, shared by POST /sync and cmd/web's
// background poller so both surface the same diff warnings and IP
// assignments: pull from syncer, semantically validate the parsed config
// (config.Validate), assign/validate static_ips against the store's prior
// snapshot (so auto-assigned addresses are stable across re-syncs — see
// ipam.Assign), diff the result against that same prior snapshot (logging
// warnings — the only place warnings are surfaced today, per Roadmap Phase 1),
// then replace the stored snapshot at time now. A failure reading prior state
// is logged and treated as no baseline — never failing the sync, since the
// diff is informational and a missing baseline only means auto-assignment
// can't reuse a prior address this one time. A nil store skips validation,
// IPAM, the diff, and the replace (sync-only mode). Errors wrap ErrSync
// (pull/parse), ErrValidate (gateway/range/static_ip semantics), ErrIPAM
// (duplicate/out-of-range/exhausted static_ip), or ErrStore (persistence).
func SyncOnce(ctx context.Context, syncer Syncer, store Store, now time.Time) (SyncResult, error) {
	cfg, sha, err := syncer.Sync(ctx)
	if err != nil {
		return SyncResult{}, fmt.Errorf("%w: %w", ErrSync, err)
	}

	var diff configdiff.Result
	if store != nil {
		oldCfg, firstSync, err := readSnapshot(ctx, store)
		if err != nil {
			log.Printf("read prior state: %v", err)
			firstSync = true
		}

		// Validate the operator-authored config before IPAM fills in any
		// auto-assigned addresses, so explicit static_ips/gateways/ranges are
		// checked as written and ipam.Assign can assume a semantically sound
		// Config (valid CIDRs, in-range explicit IPs).
		if issues := config.Validate(cfg); !issues.Empty() {
			return SyncResult{}, fmt.Errorf("%w: %w", ErrValidate, issues)
		}

		if err := ipam.Assign(cfg.Networks, cfg.Instances, oldCfg.Instances); err != nil {
			return SyncResult{}, fmt.Errorf("%w: %w", ErrIPAM, err)
		}

		if firstSync {
			log.Printf("configdiff: first sync, %d networks / %d instances baseline", len(cfg.Networks), len(cfg.Instances))
		} else {
			diff = configdiff.Diff(oldCfg, cfg)
			if !diff.Empty() {
				log.Printf("configdiff: %d/%d/%d networks added/changed/removed, %d/%d/%d instances added/changed/removed:\n%s",
					len(diff.AddedNetworks), len(diff.ChangedNetworks), len(diff.RemovedNetworks),
					len(diff.AddedInstances), len(diff.ChangedInstances), len(diff.RemovedInstances),
					strings.Join(diff.Lines(), "\n"))
			}
		}

		if err := store.Replace(ctx, cfg, sha, now); err != nil {
			return SyncResult{}, fmt.Errorf("%w: %w", ErrStore, err)
		}
	}

	return SyncResult{Commit: sha, Config: cfg, Diff: diff}, nil
}

func handleSync(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc.syncer == nil {
			http.Error(w, "sync not configured", http.StatusServiceUnavailable)
			return
		}

		res, err := svc.Sync(r.Context(), time.Now())
		if err != nil {
			// Avoid returning raw internal errors to clients; keep the
			// detail in the server log instead.
			log.Printf("sync failed: %v", err)
			if errors.Is(err, ErrStore) {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			http.Error(w, "sync failed", http.StatusBadGateway)
			return
		}

		writeJSON(w, syncResponse{
			Commit:    res.Commit,
			Networks:  len(res.Config.Networks),
			Instances: len(res.Config.Instances),
			Diff:      newDiffCounts(res.Diff),
		})
	}
}

// readSnapshot reads the store's current Networks/Instances (the
// last-synced snapshot, before the caller applies a new one via Replace)
// and reports whether no sync has happened yet (per LastSync's ok), so
// callers can skip a noisy "everything added" diff on first sync.
func readSnapshot(ctx context.Context, store Store) (cfg config.Config, firstSync bool, err error) {
	if _, _, ok, err := store.LastSync(ctx); err != nil {
		return config.Config{}, false, fmt.Errorf("query last sync: %w", err)
	} else if !ok {
		return config.Config{}, true, nil
	}

	networks, err := store.Networks(ctx)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("query networks: %w", err)
	}
	instances, err := store.Instances(ctx)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("query instances: %w", err)
	}
	return config.Config{Networks: networks, Instances: instances}, false, nil
}

type statusResponse struct {
	Synced   bool   `json:"synced"`
	Commit   string `json:"commit,omitempty"`
	SyncedAt string `json:"synced_at,omitempty"`
}

func handleStatus(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}

		commit, syncedAt, ok, err := store.LastSync(r.Context())
		if err != nil {
			log.Printf("query last sync: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if !ok {
			writeJSON(w, statusResponse{Synced: false})
			return
		}
		writeJSON(w, statusResponse{Synced: true, Commit: commit, SyncedAt: syncedAt.Format(time.RFC3339)})
	}
}

func handleNetworks(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		networks, err := store.Networks(r.Context())
		if err != nil {
			log.Printf("query networks: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		// Serve [] rather than null on an empty store so clients get a stable
		// array contract.
		if networks == nil {
			networks = []config.Network{}
		}
		writeJSON(w, networks)
	}
}

func handleInstances(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "store not configured", http.StatusServiceUnavailable)
			return
		}
		instances, err := store.Instances(r.Context())
		if err != nil {
			log.Printf("query instances: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if instances == nil {
			instances = []config.Instance{}
		}
		writeJSON(w, instances)
	}
}

// writeJSON encodes v into a buffer first so a (very unlikely) encoding
// error can still produce a clean error response instead of a partially
// written body followed by a superfluous WriteHeader call.
func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}
