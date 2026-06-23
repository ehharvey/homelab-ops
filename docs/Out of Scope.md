# Out of Scope
This page tracks work that is out-of-scope, but still worth documenting.

- Multi-node clusters; revisit wrapping Operations Center once that's real
- GitOps auto-apply and rollback (today: diff-and-warn only)
- Private repos, repo-sharing across environments
- IPv6, DHCP/DNS write-back
- Cert rotation/revocation, moving off self-signed to a real CA
- Multi-disk / multi-NIC instance overrides
- Commit-hash-per-node tracking
- Phone-home / hardware-manifest reporting over Tailscale
- Migrating the web app's own runtime from its Docker Compose/binary deployment into the IncusOS fleet
- Standardized schema output for the fleet-definition format and/or the web app's REST API: JSON Schema generated from `internal/config`'s structs (editor autocomplete, CI validation of fleet YAML) and/or OpenAPI for the existing HTTP API (see `Architecture.md` § HTTP API). No GraphQL — the app's scope is simple CRUD-ish config management, not query-heavy enough to justify a schema-first GraphQL layer. This is the expected growth path for validation if it ever outgrows the hand-rolled `config.Validate` chosen in `Open Questions.md` § Validation approach — generate the schema *from* the (typed `net/netip`) structs, rather than adopt a schema-DSL library.
- Multi-user on web app. V1 should focus on just 1 user for now. No auth needed.
