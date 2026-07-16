-- Schema for the homelab-ops web app's local store: the last-synced
-- snapshot of parsed fleet config. Lives in its own file (rather than a Go
-- string constant) so it doubles as the sql.schema input sqlc would read if
-- this package is ever migrated to generated queries.
--
-- sync_state is a single row (CHECK (id = 1)) since there is exactly one
-- fleet/remote today; adding multi-remote scoping later means dropping
-- that CHECK and widening the table's key, not a redesign.

CREATE TABLE IF NOT EXISTS sync_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    commit_sha TEXT NOT NULL,
    synced_at  TEXT NOT NULL -- RFC3339 UTC
);

CREATE TABLE IF NOT EXISTS networks (
    name                TEXT NOT NULL PRIMARY KEY,
    cidr                TEXT NOT NULL,
    gateway             TEXT NOT NULL,
    dhcp_excluded_range TEXT NOT NULL,
    dns                 TEXT NOT NULL -- JSON array of strings
);

CREATE TABLE IF NOT EXISTS instances (
    name         TEXT NOT NULL PRIMARY KEY,
    mac          TEXT NOT NULL,
    network      TEXT NOT NULL,
    static_ip    TEXT NOT NULL,
    disk         TEXT NOT NULL,
    nic          TEXT NOT NULL,
    tpm          INTEGER NOT NULL, -- 0/1
    secure_boot  INTEGER NOT NULL, -- 0/1
    applications TEXT NOT NULL, -- JSON array of strings
    tunnel_ip    TEXT NOT NULL DEFAULT '' -- WireGuard overlay address, app-assigned (internal/wireguard.AssignTunnelIPs)
);

-- wireguard_identity holds the web app's own long-lived WireGuard private
-- key (internal/wireguard.LoadOrGenerateIdentity): generated once on first
-- run, persisted thereafter. Single row, like sync_state.
CREATE TABLE IF NOT EXISTS wireguard_identity (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    private_key TEXT NOT NULL -- base64
);

-- instance_credentials holds each instance's minted-once WireGuard keypair
-- and one-time Incus bootstrap client cert (internal/nodeprovision.
-- EnsureCredential/Credential). Deliberately separate from `instances`:
-- that table is fully deleted and reinserted on every sync (see Replace),
-- and its contents are served directly by the unauthenticated
-- GET /instances route — this table must survive sync churn and must never
-- be reachable through that listing path.
CREATE TABLE IF NOT EXISTS instance_credentials (
    instance_name         TEXT NOT NULL PRIMARY KEY,
    wireguard_private_key TEXT NOT NULL, -- base64
    bootstrap_cert_pem    TEXT NOT NULL,
    bootstrap_key_pem     TEXT NOT NULL,
    created_at            TEXT NOT NULL -- RFC3339 UTC
);
