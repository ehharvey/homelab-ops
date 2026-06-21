# Out of Scope
This page tracks work that is out-of-scope, but still worth documenting.

## Architecture.md
- Multi-node clusters; revisiting Operations Center wrapping for them
- GitOps auto-apply and rollback (v1 is diff-and-warn only)
- Private GitHub repos / auth, repo-sharing across environments
- IPv6, DHCP/DNS write-back integration
- Cert rotation/revocation, CA-backed (vs. self-signed) certs
- Multi-disk / multi-NIC instance definitions
- Commit-hash-per-node tracking
- Phone-home / hardware-manifest reporting over Tailscale
- Migrating the web app's own runtime from the dev k8s cluster into the IncusOS fleet

## Roadmap.md
- Multi-node clusters; revisit wrapping Operations Center once that's real
- GitOps auto-apply + rollback (today: diff-and-warn only)
- Private repos, repo-sharing across environments
- IPv6, DHCP/DNS write-back
- Cert rotation/revocation, moving off self-signed to a real CA
- Multi-disk / multi-NIC instance overrides
- Commit-hash-per-node tracking
- Phone-home / hardware-manifest reporting over Tailscale
- Migrating the web app's own runtime off the dev k8s cluster and into the IncusOS fleet
- Standardized schema output for the fleet-definition format and/or the future web app API: JSON Schema generated from `internal/config`'s structs (editor autocomplete, CI validation of fleet YAML) and/or OpenAPI for the web app's REST API once Phase 1+ has handlers to annotate. No GraphQL — the app's scope is simple CRUD-ish config management, not query-heavy enough to justify a schema-first GraphQL layer.
- Multi-user on web app. V1 should focus on just 1 user for now. No auth needed.
