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
    applications TEXT NOT NULL -- JSON array of strings
);
