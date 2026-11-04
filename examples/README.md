# Examples

Two kinds, kept apart because they're read differently:

- **`nodes/`**: fleet config repos, the YAML the bootstrap CLI, the web app
  and the agents read. The field reference is
  [`docs/Config Schema.md`](../docs/Config%20Schema.md).
- **`webapp/`**: a deployment config directory, the web app's own settings
  (`DEPLOYMENT_CONFIG_DIR`). The reference is
  [`docs/Deployment.md`](../docs/Deployment.md).

| Example | For | Shows |
|---|---|---|
| [`nodes/single-node/`](nodes/single-node/) | `bootstrap render-seed --file examples/nodes/single-node/fleet.yaml` | The CLI's input: exactly one Network and one Instance, with a static IP so the node comes up as a one-member cluster. |
| [`nodes/fleet/`](nodes/fleet/) | A config repo the web app and the agents sync (`CONFIG_REPO_URL`) | Three nodes across four files: one pinned `static_ip` and two assigned by IPAM, plus the per-node agent App and the Designation naming which node's agent leads. |
| [`webapp/`](webapp/) | The web app's `DEPLOYMENT_CONFIG_DIR` | The default, tunnel-only posture: `web.env`, an `operator-peers` template, and where `client.crt` and `secrets/` go. No key is committed. |

Each `nodes/` directory is laid out like a repo root: only root-level `*.yaml`
and `*.yml` files count, merged in file-name order.

To sync `nodes/fleet/`, copy its files to the root of a git repo and point
`CONFIG_REPO_URL` at it. `make dev` uses its own fixture instead
(`dev/git-fixture/fleet.yaml`), whose values the validate scripts assert
against.

**These are tested.** `internal/config`'s `TestExamplesParseAndValidate`
reads every directory under `nodes/` the way a sync reads a repo, and fails if
any of them no longer parses or validates. `TestRenderSeedCommandRendersSingleNodeExample`
also renders `nodes/single-node/` through the CLI. A new directory under
`nodes/` is picked up with no test changes. Anything in the Proposed section
of the schema doc can't appear there until it's implemented, because the
parser rejects it. `internal/deployconfig`'s `TestExampleWebappLoads` loads
`webapp/`, and `scripts/validate/web-starts-from-deployment-config-directory.sh`
starts the real compose stack from a copy of it.
