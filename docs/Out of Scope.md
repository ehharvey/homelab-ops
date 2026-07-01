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
- Running the web container as a non-root user (defense-in-depth): switch the final Dockerfile stage to distroless's `nonroot` user (uid 65532) / the `:nonroot` image tag. Tackle after Phase 3 — 0.x is single-user/no-auth (below), so root-in-container isn't a headline risk yet. Read-only config mounts (e.g. `CLIENT_CERT_PATH`) and `/tmp` (mode 1777) already work as non-root; the one thing to verify first is that a mounted `STORE_PATH` volume (the sqlite file) is writable by uid 65532, or the store fails to open on startup.
- Standardized schema output for the fleet-definition format and/or the web app's REST API: JSON Schema generated from `internal/config`'s structs (editor autocomplete, CI validation of fleet YAML) and/or OpenAPI for the existing HTTP API (see `Architecture.md` § HTTP API). No GraphQL — the app's scope is simple CRUD-ish config management, not query-heavy enough to justify a schema-first GraphQL layer. This is the expected growth path for validation if it ever outgrows the hand-rolled `config.Validate` chosen in `Decisions.md` § Validation approach — generate the schema *from* the (typed `net/netip`) structs, rather than adopt a schema-DSL library.
- Multi-user on web app. 0.x should focus on just 1 user for now. No auth needed.
