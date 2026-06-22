// Package store persists the most recently synced fleet config so it can
// be queried by kind/name across sync runs. It is the storage layer
// docs/Architecture.md flagged as "not yet specified" for #21.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	_ "modernc.org/sqlite" // pure-Go sqlite driver, registers "sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

const timeLayout = time.RFC3339

// Store holds the last-synced Config snapshot plus sync metadata
// (commit SHA, sync time), queryable by kind/name.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) a store at path. Use ":memory:" for an
// in-process, non-persistent store; any other value is a file path,
// created if it doesn't already exist.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// sqlite only supports one writer at a time; serialize through a
	// single connection rather than fighting the pool over SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		db.Close() //nolint:errcheck,gosec // best-effort cleanup on a failed Open
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Replace atomically replaces the stored snapshot with cfg, recording
// commit as the synced commit SHA and now as the sync time. Config sync
// always produces a full Config, never an incremental delta, so each
// successful sync fully replaces prior state rather than merging into it.
// A duplicate name within cfg is last-one-wins, logged as a warning —
// full validation/warning surfacing belongs to #22, not this store.
func (s *Store) Replace(ctx context.Context, cfg config.Config, commit string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx, deleteNetworksSQL); err != nil {
		return fmt.Errorf("clear networks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, deleteInstancesSQL); err != nil {
		return fmt.Errorf("clear instances: %w", err)
	}

	seen := make(map[string]bool, len(cfg.Networks))
	for _, n := range cfg.Networks {
		if seen[n.Name] {
			log.Printf("store: duplicate network %q in synced config, last one wins", n.Name)
		}
		seen[n.Name] = true

		dns, err := json.Marshal(n.DNS)
		if err != nil {
			return fmt.Errorf("marshal network %q dns: %w", n.Name, err)
		}
		if _, err := tx.ExecContext(ctx, replaceNetworkSQL, n.Name, n.CIDR, n.Gateway, n.DHCPExcludedRange, string(dns)); err != nil {
			return fmt.Errorf("store network %q: %w", n.Name, err)
		}
	}

	seen = make(map[string]bool, len(cfg.Instances))
	for _, i := range cfg.Instances {
		if seen[i.Name] {
			log.Printf("store: duplicate instance %q in synced config, last one wins", i.Name)
		}
		seen[i.Name] = true

		apps, err := json.Marshal(i.Applications)
		if err != nil {
			return fmt.Errorf("marshal instance %q applications: %w", i.Name, err)
		}
		if _, err := tx.ExecContext(ctx, replaceInstanceSQL,
			i.Name, i.MAC, i.Network, i.StaticIP, i.Disk, i.NIC,
			boolToInt(i.Security.TPM), boolToInt(i.Security.SecureBoot), string(apps),
		); err != nil {
			return fmt.Errorf("store instance %q: %w", i.Name, err)
		}
	}

	if _, err := tx.ExecContext(ctx, upsertSyncStateSQL, commit, now.UTC().Format(timeLayout)); err != nil {
		return fmt.Errorf("store sync state: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// LastSync reports the most recently recorded commit SHA and sync time.
// ok is false if no sync has ever completed.
func (s *Store) LastSync(ctx context.Context) (commit string, syncedAt time.Time, ok bool, err error) {
	var rawTime string
	err = s.db.QueryRowContext(ctx, getSyncStateSQL).Scan(&commit, &rawTime)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", time.Time{}, false, nil
		}
		return "", time.Time{}, false, fmt.Errorf("query sync state: %w", err)
	}
	syncedAt, err = time.Parse(timeLayout, rawTime)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("parse synced_at: %w", err)
	}
	return commit, syncedAt, true, nil
}

// Network returns the stored Network named name, or ok=false if absent.
func (s *Store) Network(ctx context.Context, name string) (n config.Network, ok bool, err error) {
	n, err = scanNetwork(s.db.QueryRowContext(ctx, getNetworkSQL, name))
	if err != nil {
		if err == sql.ErrNoRows {
			return config.Network{}, false, nil
		}
		return config.Network{}, false, err
	}
	return n, true, nil
}

// Networks returns every stored Network, ordered by name.
func (s *Store) Networks(ctx context.Context) ([]config.Network, error) {
	rows, err := s.db.QueryContext(ctx, listNetworksSQL)
	if err != nil {
		return nil, fmt.Errorf("query networks: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query, nothing to flush

	var out []config.Network
	for rows.Next() {
		n, err := scanNetwork(rows)
		if err != nil {
			return nil, fmt.Errorf("scan network: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Instance returns the stored Instance named name, or ok=false if absent.
func (s *Store) Instance(ctx context.Context, name string) (i config.Instance, ok bool, err error) {
	i, err = scanInstance(s.db.QueryRowContext(ctx, getInstanceSQL, name))
	if err != nil {
		if err == sql.ErrNoRows {
			return config.Instance{}, false, nil
		}
		return config.Instance{}, false, err
	}
	return i, true, nil
}

// Instances returns every stored Instance, ordered by name.
func (s *Store) Instances(ctx context.Context) ([]config.Instance, error) {
	rows, err := s.db.QueryContext(ctx, listInstancesSQL)
	if err != nil {
		return nil, fmt.Errorf("query instances: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query, nothing to flush

	var out []config.Instance
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// row is the subset of *sql.Row/*sql.Rows that scanNetwork/scanInstance need.
type row interface {
	Scan(dest ...any) error
}

func scanNetwork(r row) (config.Network, error) {
	var n config.Network
	var dns string
	if err := r.Scan(&n.Name, &n.CIDR, &n.Gateway, &n.DHCPExcludedRange, &dns); err != nil {
		return config.Network{}, err
	}
	if err := json.Unmarshal([]byte(dns), &n.DNS); err != nil {
		return config.Network{}, fmt.Errorf("unmarshal dns for %q: %w", n.Name, err)
	}
	return n, nil
}

func scanInstance(r row) (config.Instance, error) {
	var i config.Instance
	var tpm, secureBoot int
	var apps string
	if err := r.Scan(&i.Name, &i.MAC, &i.Network, &i.StaticIP, &i.Disk, &i.NIC, &tpm, &secureBoot, &apps); err != nil {
		return config.Instance{}, err
	}
	i.Security.TPM = tpm != 0
	i.Security.SecureBoot = secureBoot != 0
	if err := json.Unmarshal([]byte(apps), &i.Applications); err != nil {
		return config.Instance{}, fmt.Errorf("unmarshal applications for %q: %w", i.Name, err)
	}
	return i, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
