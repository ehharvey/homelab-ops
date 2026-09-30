# Examples

Config repos that parse and validate against today's schema. The field
reference is [`docs/Config Schema.md`](../docs/Config%20Schema.md).

| Example | For | Shows |
|---|---|---|
| [`single-node/`](single-node/) | `bootstrap render-seed --file examples/single-node/fleet.yaml` | The CLI's input: exactly one Network and one Instance, with a static IP so the node comes up as a one-member cluster. |
| [`fleet/`](fleet/) | A config repo the web app and the agents sync (`CONFIG_REPO_URL`) | Three nodes across four files: one pinned `static_ip` and two assigned by IPAM, plus the per-node agent App and the Designation naming which node's agent leads. |

Each directory is laid out like a repo root: only root-level `*.yaml` and
`*.yml` files count, merged in file-name order.

To sync `fleet/`, copy its files to the root of a git repo and point
`CONFIG_REPO_URL` at it. `make dev` uses its own fixture instead
(`dev/git-fixture/fleet.yaml`), whose values the validate scripts assert
against.

**These are tested.** `internal/config`'s `TestExamplesParseAndValidate`
reads every directory here the way a sync reads a repo, and fails if any of
them no longer parses or validates. `TestRenderSeedCommandRendersSingleNodeExample`
also renders `single-node/` through the CLI. A new example directory is picked
up with no test changes. Anything in the Proposed section of the schema doc
can't appear here until it's implemented, because the parser rejects it.
