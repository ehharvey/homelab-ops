package store

// Queries are written as standalone, named, parameterized SQL strings —
// the shape sqlc would generate from a query.sql file — so that adopting
// sqlc later (if the query surface grows) is a mechanical move into a
// query.sql file plus `sqlc generate`, not a rewrite.

// -- name: DeleteNetworks :exec
const deleteNetworksSQL = `DELETE FROM networks`

// -- name: DeleteInstances :exec
const deleteInstancesSQL = `DELETE FROM instances`

// -- name: ReplaceNetwork :exec
const replaceNetworkSQL = `
INSERT OR REPLACE INTO networks (name, cidr, gateway, dhcp_excluded_range, dns)
VALUES (?, ?, ?, ?, ?)`

// -- name: ReplaceInstance :exec
const replaceInstanceSQL = `
INSERT OR REPLACE INTO instances (name, mac, network, static_ip, disk, nic, tpm, secure_boot, applications)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// -- name: UpsertSyncState :exec
const upsertSyncStateSQL = `
INSERT INTO sync_state (id, commit_sha, synced_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET commit_sha = excluded.commit_sha, synced_at = excluded.synced_at`

// -- name: GetSyncState :one
const getSyncStateSQL = `SELECT commit_sha, synced_at FROM sync_state WHERE id = 1`

// -- name: ListNetworks :many
const listNetworksSQL = `
SELECT name, cidr, gateway, dhcp_excluded_range, dns FROM networks ORDER BY name`

// -- name: GetNetwork :one
const getNetworkSQL = `
SELECT name, cidr, gateway, dhcp_excluded_range, dns FROM networks WHERE name = ?`

// -- name: ListInstances :many
const listInstancesSQL = `
SELECT name, mac, network, static_ip, disk, nic, tpm, secure_boot, applications FROM instances ORDER BY name`

// -- name: GetInstance :one
const getInstanceSQL = `
SELECT name, mac, network, static_ip, disk, nic, tpm, secure_boot, applications FROM instances WHERE name = ?`
