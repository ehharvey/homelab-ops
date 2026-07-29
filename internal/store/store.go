// Package store persists the most recently synced fleet config so it can
// be queried by kind/name across sync runs. It is the storage layer
// docs/Architecture.md flagged as "not yet specified" for #21.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/wireguard"
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
	if _, err := tx.ExecContext(ctx, deleteAppsSQL); err != nil {
		return fmt.Errorf("clear apps: %w", err)
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
		if _, err := tx.ExecContext(ctx, replaceNetworkSQL, n.Name, asText(n.CIDR), asText(n.Gateway), asText(n.DHCPExcludedRange), string(dns)); err != nil {
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
			i.Name, i.MAC, i.Network, asText(i.StaticIP), i.Disk, i.NIC,
			boolToInt(i.Security.TPM), boolToInt(i.Security.SecureBoot), string(apps), asText(i.TunnelIP),
		); err != nil {
			return fmt.Errorf("store instance %q: %w", i.Name, err)
		}
	}

	seen = make(map[string]bool, len(cfg.Apps))
	for _, a := range cfg.Apps {
		if seen[a.Name] {
			log.Printf("store: duplicate app %q in synced config, last one wins", a.Name)
		}
		seen[a.Name] = true

		params, err := json.Marshal(a.Params)
		if err != nil {
			return fmt.Errorf("marshal app %q params: %w", a.Name, err)
		}
		if _, err := tx.ExecContext(ctx, replaceAppSQL,
			a.Name, a.Type, asText(a.Replicas),
			a.Image.Server, a.Image.Protocol, a.Image.Alias, a.Image.Fingerprint, string(params),
		); err != nil {
			return fmt.Errorf("store app %q: %w", a.Name, err)
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

// Apps returns every stored App, ordered by name.
func (s *Store) Apps(ctx context.Context) ([]config.App, error) {
	rows, err := s.db.QueryContext(ctx, listAppsSQL)
	if err != nil {
		return nil, fmt.Errorf("query apps: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query, nothing to flush

	var out []config.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
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
	var cidr, gateway, excluded, dns string
	if err := r.Scan(&n.Name, &cidr, &gateway, &excluded, &dns); err != nil {
		return config.Network{}, err
	}
	if err := n.CIDR.UnmarshalText([]byte(cidr)); err != nil {
		return config.Network{}, fmt.Errorf("parse cidr for %q: %w", n.Name, err)
	}
	if err := n.Gateway.UnmarshalText([]byte(gateway)); err != nil {
		return config.Network{}, fmt.Errorf("parse gateway for %q: %w", n.Name, err)
	}
	if err := n.DHCPExcludedRange.UnmarshalText([]byte(excluded)); err != nil {
		return config.Network{}, fmt.Errorf("parse dhcp_excluded_range for %q: %w", n.Name, err)
	}
	if err := json.Unmarshal([]byte(dns), &n.DNS); err != nil {
		return config.Network{}, fmt.Errorf("unmarshal dns for %q: %w", n.Name, err)
	}
	return n, nil
}

func scanInstance(r row) (config.Instance, error) {
	var i config.Instance
	var tpm, secureBoot int
	var staticIP, apps, tunnelIP string
	if err := r.Scan(&i.Name, &i.MAC, &i.Network, &staticIP, &i.Disk, &i.NIC, &tpm, &secureBoot, &apps, &tunnelIP); err != nil {
		return config.Instance{}, err
	}
	if err := i.StaticIP.UnmarshalText([]byte(staticIP)); err != nil {
		return config.Instance{}, fmt.Errorf("parse static_ip for %q: %w", i.Name, err)
	}
	i.Security.TPM = tpm != 0
	i.Security.SecureBoot = secureBoot != 0
	if err := json.Unmarshal([]byte(apps), &i.Applications); err != nil {
		return config.Instance{}, fmt.Errorf("unmarshal applications for %q: %w", i.Name, err)
	}
	if err := i.TunnelIP.UnmarshalText([]byte(tunnelIP)); err != nil {
		return config.Instance{}, fmt.Errorf("parse tunnel_ip for %q: %w", i.Name, err)
	}
	return i, nil
}

func scanApp(r row) (config.App, error) {
	var a config.App
	var replicas, params string
	if err := r.Scan(&a.Name, &a.Type, &replicas,
		&a.Image.Server, &a.Image.Protocol, &a.Image.Alias, &a.Image.Fingerprint, &params); err != nil {
		return config.App{}, err
	}
	if err := a.Replicas.UnmarshalText([]byte(replicas)); err != nil {
		return config.App{}, fmt.Errorf("parse replicas for %q: %w", a.Name, err)
	}
	if err := json.Unmarshal([]byte(params), &a.Params); err != nil {
		return config.App{}, fmt.Errorf("unmarshal params for %q: %w", a.Name, err)
	}
	return a, nil
}

// WireGuardPrivateKey returns the web app's own persisted WireGuard
// identity, or ok=false if none has been generated yet. Implements
// internal/wireguard.IdentityStore.
func (s *Store) WireGuardPrivateKey(ctx context.Context) (wireguard.PrivateKey, bool, error) {
	var b64 string
	err := s.db.QueryRowContext(ctx, getWireGuardIdentitySQL).Scan(&b64)
	if err != nil {
		if err == sql.ErrNoRows {
			return wireguard.PrivateKey{}, false, nil
		}
		return wireguard.PrivateKey{}, false, fmt.Errorf("query wireguard identity: %w", err)
	}
	key, err := decodeKey(b64)
	if err != nil {
		return wireguard.PrivateKey{}, false, fmt.Errorf("decode wireguard identity: %w", err)
	}
	return key, true, nil
}

// SetWireGuardPrivateKey persists the web app's own WireGuard identity.
// Implements internal/wireguard.IdentityStore.
func (s *Store) SetWireGuardPrivateKey(ctx context.Context, key wireguard.PrivateKey) error {
	if _, err := s.db.ExecContext(ctx, setWireGuardIdentitySQL, key.Base64()); err != nil {
		return fmt.Errorf("store wireguard identity: %w", err)
	}
	return nil
}

// InstanceCredential returns name's persisted Credential, or ok=false if
// none has been minted yet. Implements
// internal/nodeprovision.CredentialStore.
func (s *Store) InstanceCredential(ctx context.Context, name string) (nodeprovision.Credential, bool, error) {
	var wgKeyB64, certPEM, keyPEM string
	err := s.db.QueryRowContext(ctx, getInstanceCredentialSQL, name).Scan(&wgKeyB64, &certPEM, &keyPEM)
	if err != nil {
		if err == sql.ErrNoRows {
			return nodeprovision.Credential{}, false, nil
		}
		return nodeprovision.Credential{}, false, fmt.Errorf("query credential for %q: %w", name, err)
	}
	wgKey, err := decodeKey(wgKeyB64)
	if err != nil {
		return nodeprovision.Credential{}, false, fmt.Errorf("decode wireguard key for %q: %w", name, err)
	}
	return nodeprovision.Credential{
		WireGuardPrivateKey: wgKey,
		BootstrapCertPEM:    []byte(certPEM),
		BootstrapKeyPEM:     []byte(keyPEM),
	}, true, nil
}

// SetInstanceCredential persists name's Credential. Implements
// internal/nodeprovision.CredentialStore.
func (s *Store) SetInstanceCredential(ctx context.Context, name string, cred nodeprovision.Credential) error {
	_, err := s.db.ExecContext(ctx, setInstanceCredentialSQL,
		name, cred.WireGuardPrivateKey.Base64(), string(cred.BootstrapCertPEM), string(cred.BootstrapKeyPEM),
		time.Now().UTC().Format(timeLayout),
	)
	if err != nil {
		return fmt.Errorf("store credential for %q: %w", name, err)
	}
	return nil
}

// decodeKey parses a base64-encoded 32-byte key, the encoding both
// wireguard_identity.private_key and instance_credentials.
// wireguard_private_key use (wireguard.PrivateKey.Base64).
func decodeKey(b64 string) (wireguard.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return wireguard.PrivateKey{}, err
	}
	if len(raw) != 32 {
		return wireguard.PrivateKey{}, fmt.Errorf("key has length %d, want 32", len(raw))
	}
	var key wireguard.PrivateKey
	copy(key[:], raw)
	return key, nil
}

// asText renders a netip.Addr/netip.Prefix/config.Range to the string stored
// in its TEXT column. Each marshals its zero value to "" (and UnmarshalText
// reads "" back to the zero value), so an unset optional — a DHCP instance's
// static_ip, a gateway-less network's gateway — round-trips losslessly.
// MarshalText on these stdlib/typed values never returns an error.
func asText(m encoding.TextMarshaler) string {
	b, _ := m.MarshalText()
	return string(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
