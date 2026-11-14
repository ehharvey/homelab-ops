package store

// Queries are written as standalone, named, parameterized SQL strings —
// the shape sqlc would generate from a query.sql file — so that adopting
// sqlc later (if the query surface grows) is a mechanical move into a
// query.sql file plus `sqlc generate`, not a rewrite.

// -- name: DeleteNetworks :exec
const deleteNetworksSQL = `DELETE FROM networks`

// -- name: DeleteInstances :exec
const deleteInstancesSQL = `DELETE FROM instances`

// -- name: DeleteApps :exec
const deleteAppsSQL = `DELETE FROM apps`

// -- name: ReplaceNetwork :exec
const replaceNetworkSQL = `
INSERT OR REPLACE INTO networks (name, cidr, gateway, dhcp_excluded_range, dns, dhcp_mode, dhcp_pool)
VALUES (?, ?, ?, ?, ?, ?, ?)`

// -- name: ReplaceInstance :exec
const replaceInstanceSQL = `
INSERT OR REPLACE INTO instances (name, mac, network, static_ip, disk, nic, tpm, secure_boot, applications, tunnel_ip, runtime)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// -- name: ReplaceApp :exec
const replaceAppSQL = `
INSERT OR REPLACE INTO apps (name, type, replicas, image_server, image_protocol, image_alias, image_fingerprint, params)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// -- name: UpsertSyncState :exec
const upsertSyncStateSQL = `
INSERT INTO sync_state (id, commit_sha, synced_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET commit_sha = excluded.commit_sha, synced_at = excluded.synced_at`

// -- name: GetSyncState :one
const getSyncStateSQL = `SELECT commit_sha, synced_at FROM sync_state WHERE id = 1`

// -- name: ListNetworks :many
const listNetworksSQL = `
SELECT name, cidr, gateway, dhcp_excluded_range, dns, dhcp_mode, dhcp_pool FROM networks ORDER BY name`

// -- name: GetNetwork :one
const getNetworkSQL = `
SELECT name, cidr, gateway, dhcp_excluded_range, dns, dhcp_mode, dhcp_pool FROM networks WHERE name = ?`

// -- name: ListInstances :many
const listInstancesSQL = `
SELECT name, mac, network, static_ip, disk, nic, tpm, secure_boot, applications, tunnel_ip, runtime FROM instances ORDER BY name`

// -- name: ListInstanceTunnelIPs :many
const listInstanceTunnelIPsSQL = `
SELECT name, tunnel_ip FROM instances ORDER BY name`

// -- name: GetInstance :one
const getInstanceSQL = `
SELECT name, mac, network, static_ip, disk, nic, tpm, secure_boot, applications, tunnel_ip, runtime FROM instances WHERE name = ?`

// -- name: ListApps :many
const listAppsSQL = `
SELECT name, type, replicas, image_server, image_protocol, image_alias, image_fingerprint, params FROM apps ORDER BY name`

// -- name: GetInstanceCredential :one
//
//nolint:gosec // G101: SQL column names (wireguard_private_key/bootstrap_key_pem), not an embedded credential
const getInstanceCredentialSQL = `
SELECT wireguard_private_key, bootstrap_cert_pem, bootstrap_key_pem FROM instance_credentials WHERE instance_name = ?`

// -- name: SetInstanceCredential :exec
// ON CONFLICT DO NOTHING, not OR REPLACE: nodeprovision.EnsureCredential
// relies on this only ever inserting once per instance_name, then
// re-reading, to stay correct when a manual seed/image fetch races the
// background sync poller's reconcile pass for the same brand-new
// instance — see its doc comment.
//
//nolint:gosec // G101: SQL column names (wireguard_private_key/bootstrap_key_pem), not an embedded credential
const setInstanceCredentialSQL = `
INSERT INTO instance_credentials (instance_name, wireguard_private_key, bootstrap_cert_pem, bootstrap_key_pem, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (instance_name) DO NOTHING`

// -- name: CountColumn :one
// pragma_table_info is SQLite's table-valued form of PRAGMA table_info, so the
// table name can be a parameter.
const countColumnSQL = `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`

// -- name: AddInstancesRuntime :exec
const addInstancesRuntimeSQL = `ALTER TABLE instances ADD COLUMN runtime TEXT NOT NULL DEFAULT ''`

// -- name: AddNetworksDHCPMode :exec
const addNetworksDHCPModeSQL = `ALTER TABLE networks ADD COLUMN dhcp_mode TEXT NOT NULL DEFAULT ''`

// -- name: AddNetworksDHCPPool :exec
const addNetworksDHCPPoolSQL = `ALTER TABLE networks ADD COLUMN dhcp_pool TEXT NOT NULL DEFAULT ''`
