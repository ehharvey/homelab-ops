# Deployment config

The web app's deployment config contract (#241, `Decisions.md` §29): every
setting `cmd/web` reads, its default, and the directory that holds them on an
appliance. Every host form runs the same published image
(`ghcr.io/ehharvey/homelab-ops/web`) with this same directory, so moving
between hosts is a copy, not a rebuild. Each setting is an operator choice
with a secure default.

`internal/deployconfig` is the source of truth. Its `Settings` table is the
contract, and tests hold `cmd/web` to it: `cmd/web` reads no environment
variable itself, `deployconfig.Load` reads no name the table doesn't list, and
every row of the table has a row in [§ Settings](#settings) below.

A working example is [`examples/webapp/`](https://github.com/ehharvey/homelab-ops/tree/main/examples/webapp).
The fleet config repos (what the web app *syncs*) are a separate thing, in
[`examples/nodes/`](https://github.com/ehharvey/homelab-ops/tree/main/examples/nodes)
and `docs/Config Schema.md`.

## The directory

`DEPLOYMENT_CONFIG_DIR` names it, for example `/etc/homelab-ops/web/`, mounted
read-only into the container. Unset, the web app reads the environment alone,
as dev and most validate scripts do.

```text
/etc/homelab-ops/web/       DEPLOYMENT_CONFIG_DIR
├── web.env                 scalar settings, KEY=VALUE
├── client.crt              the break-glass cert's public half
├── operator-peers          operator devices allowed on the tunnel API
└── secrets/                0700: every secret file, each 0600
    └── wireguard.key       the web app's WireGuard identity (#197)
```

| Entry | Setting | Secret | Mode | Contents |
| --- | --- | --- | --- | --- |
| `web.env` | every scalar setting below | no | `0644` is fine | `KEY=VALUE` lines, the same names as the env vars. Optional. |
| `client.crt` | `CLIENT_CERT_PATH` | no | `0644` is fine | The break-glass client cert's **public** half, from `bootstrap gen-cert` (§4). The web app never reads the private key, and it must not be in this directory. Optional: without it the seed and image routes report themselves unconfigured. |
| `operator-peers` | `WIREGUARD_OPERATOR_PEERS` | no | `0644` is fine | One `<overlay-address>=<base64-public-key>` per line; `#` starts a comment. Only public keys. Optional. |
| `secrets/` | | yes | `0700` | Everything secret, and nothing else. A new secret goes here with no list to update. |
| `secrets/wireguard.key` | (reserved, #197) | yes | `0600` | The web app's operator-supplied WireGuard identity (§28). **Reserved:** today's identity is still generated into the store, and #197 loads this file and exports the store's `wireguard_identity` row to it. |

Rules `deployconfig.Load` enforces at startup:

- **The environment overrides the directory**, setting by setting, and that
  includes the three files: `CLIENT_CERT_PATH` and `WIREGUARD_OPERATOR_PEERS`
  in the environment win over `client.crt` and `operator-peers`. An env var
  set to the **empty string** still overrides: the setting then takes its
  default, as if neither set it. So dev and the validate suite, which set env
  vars, are unaffected by a directory.
- **`web.env` takes only the scalar settings.** An unknown key fails startup,
  so a typo can't be silently ignored. So do the file-backed settings
  (`CLIENT_CERT_PATH`, `WIREGUARD_OPERATOR_PEERS`: each has its own file) and
  `DEPLOYMENT_CONFIG_DIR` itself. Other files in the directory are ignored.
- **`secrets/` is held to its modes.** If it exists, it and everything under
  it must have no group or other permission bits (`0700` for directories,
  `0600` or stricter for files), or startup fails naming each one. Only
  permission bits are checked, not ownership. It may be absent: nothing reads
  it yet, and in TPM mode (#509) there is no identity file at all.
- **`BASE_IMAGE_PATH` points outside the directory.** The base image is
  multi-GB, and #243 carries the directory inline in cloud-init `user-data`.

### `web.env`'s format

`web.env` is read with viper's dotenv codec (`github.com/subosito/gotenv`),
and its rules are the format:

- `KEY=VALUE`, one per line. Blank lines and lines starting with `#` are
  skipped; an optional leading `export ` is allowed.
- `"double quotes"` allow spaces and the escapes `\n`, `\r` and `\"`.
- `'single quotes'` are literal.
- **`$NAME` and `${NAME}` are expanded** from the web app's environment in
  unquoted and double-quoted values. Single-quote a value that contains a
  literal `$`.
- A later line for the same key replaces an earlier one.

## Settings

Every setting `cmd/web` reads. "Unset" and "empty" mean the same thing for the
value; the precedence rule above is about *who* sets it. The web app exits
non-zero at startup, naming every missing or invalid setting in one message,
rather than failing later where a setting is first used (#67).

| Setting | Default | Where | Meaning, and what fails startup |
| --- | --- | --- | --- |
| `DEPLOYMENT_CONFIG_DIR` | unset (environment only) | env only | The directory above. Fails if it doesn't exist or isn't a directory. |
| `WIREGUARD_ENDPOINT` | unset (no tunnel) | `web.env` | The `host:port` nodes dial to reach the tunnel; it is baked into every node's seed. No default: the operator supplies it. **Recommended: a DNS name** (see below). Setting it starts the tunnel, where the API is served at `10.100.0.1:80`. **Required when operator peers are listed.** Fails unless it is `host:port` with a host and a numeric port. |
| `WIREGUARD_PORT` | `51820` | `web.env` | The tunnel's UDP listen port inside the container (`0` lets the OS pick). Fails unless a number from 0 to 65535. |
| `WIREGUARD_OPERATOR_PEERS` | empty | `operator-peers` file | The operator devices allowed on the tunnel API: `<overlay-address>=<base64-public-key>` entries, separated by commas or whitespace in the env var, one per line in the file. Each address must be in `10.100.0.240`–`10.100.0.254` (`wireguard.OperatorCIDR`), and no address or key may repeat. Fails on any bad entry. |
| `API_HOST_LISTEN_ADDR` | unset (off) | `web.env` | **Opt-in.** Also serves the whole API as plain HTTP on this address, with no source filter; startup logs a warning. Fails unless `host:port` or `:port`. |
| `HEALTH_LISTEN_ADDR` | `:8081` | `web.env` | Serves `GET /healthz` only, whatever the API listener. Fails unless `host:port` or `:port`. |
| `PORT` | unset | `web.env` | Retired by #195 and not read. Fails if set with no API listener (neither `WIREGUARD_ENDPOINT` nor `API_HOST_LISTEN_ADDR`), since such a deployment used to serve the API on it; otherwise only logged. |
| `CONFIG_REPO_URL` | unset (sync off) | `web.env` | The fleet config repo, any go-git transport. **Fails if it carries a credential**, without echoing it: any userinfo on a non-SSH URL (`https://user:token@…`, `https://token@…`), or a password on an SSH one (`ssh://user:pass@…`, `user:pass@host:path`). An SSH user alone (`git@host:path`, `ssh://git@host/…`) is a login name, not a secret, so it's accepted; the key comes from ssh-agent. Credentials never go in the URL or `web.env`. Git credentials are #218's, under `secrets/`. |
| `CONFIG_REPO_REF` | `main` | `web.env` | The branch to sync. |
| `CONFIG_SYNC_INTERVAL` | unset (sync on `POST /sync` only) | `web.env` | Background poll interval, a Go duration such as `5m`. Fails unless a positive duration. |
| `CONFIG_REPO_PRIVATE` | `false` (git addressing rejected) | `web.env` | Declares the config repo private, so a `kind: Network` in it may carry its addressing (§28, #199). Unset, the repo is assumed public: a sync fails on any Network with a `cidr` in git, naming it. Fails startup unless `true` or `false`. |
| `NETWORK_ADDRESSING_PATH` | unset (no state addressing) | `web.env` | A file of full `kind: Network` documents addressing the networks git names without a `cidr` (§28, #199), read on every sync. Unset, a name-only Network fails the sync. A path, inside the directory or not. Fails if set and not a regular file, or if it doesn't parse. Interim: the encrypted store, loaded by the CLI, replaces it (#514). |
| `STORE_PATH` | `:memory:` | `web.env` | The sqlite store's file. The default keeps nothing across a restart; an appliance mounts a volume and sets a path on it. |
| `CLIENT_CERT_PATH` | unset (seed and image routes off) | `client.crt` file | The break-glass cert's public half (§4). Fails if set and not a regular file. |
| `BASE_IMAGE_PATH` | unset (server-side image builds off) | `web.env` | The operator-supplied base IncusOS raw image; setting it turns on the image route (§29's server-side default). A path outside the directory. Fails if set and not a regular file. |
| `FLASHER_TOOL_PATH` | `flasher-tool` from `$PATH`; the image sets `/flasher-tool` | `web.env` | The `flasher-tool` binary the image route runs. Only dev outside the image needs it. |

Two behaviour changes came with fail-fast (#241). Before, an invalid
`WIREGUARD_PORT` was logged and replaced by `51820`, and an invalid
`CONFIG_SYNC_INTERVAL` was logged and turned the poller off. Both now stop
startup. A missing `CLIENT_CERT_PATH` or `BASE_IMAGE_PATH` file used to show up
only on the first request.

### The WireGuard endpoint as a DNS name

`WIREGUARD_ENDPOINT` is baked into every node's seed, so an IP address pins
the web app to one host: moving it means reflashing every node. A DNS name
lets a migration (§29, #245) repoint the name instead. IncusOS accepts a
hostname: `networkd_validate.go` checks it with `net.SplitHostPort`, and it is
rendered into systemd-networkd's `Endpoint=`. **Unverified:** whether a running
node re-resolves the name after its address changes, or only at boot. #245
probes it.

## What is secret, and where

- **On disk in plaintext:** only an operator-supplied WireGuard identity,
  `secrets/wireguard.key` (§28). In TPM mode (#509) the identity is generated
  and only ever sealed, so there is no file.
- **Not in this directory at all:** the store key (operator-held, entered at
  unlock, §28), and step-ca's intermediate password and kanidm's admin
  credentials (§32), which live only in the encrypted store.
- **Public, so not under `secrets/`:** `client.crt` and `operator-peers`,
  and everything in `web.env`. A `CONFIG_REPO_URL` with credentials fails
  startup, so `web.env` can't hold one.
- **Keep `secrets/` out of anything snapshot-shaped** (§28, §32): snapshots
  and backups of the web app never include it. The operator keeps their own
  backup of the identity.
- #243's `--no-secrets` leaves out exactly `secrets/`.

## Every §29 setting

§29's operator-choice table, row by row. The rows that are `cmd/web` settings
point at the table above; the rest are chosen somewhere else and get no env
var here.

| §29 row | Default | Where it is set |
| --- | --- | --- |
| Host form | Stock Debian cloud image set up by cloud-init | #243's `user-data` and compose file; not a web app setting |
| Host OS updates | `unattended-upgrades`, rebooting only when required, in a window | #243: the reboot window is a `user-data` setting |
| How the config directory reaches the host | Inline in cloud-init `user-data`, the WireGuard key included | #243: inline, or `--no-secrets`, which leaves out `secrets/` for the operator to copy in after boot |
| API listener | Tunnel only, operator peers | `WIREGUARD_ENDPOINT`, `operator-peers`, `API_HOST_LISTEN_ADDR` above. TLS from step-ca is #506's, and adds its own settings |
| Where install media is built | Server-side, when `BASE_IMAGE_PATH` is set | `BASE_IMAGE_PATH` above. Client-side builds are #242's |
| node0 provisioning | By the web app over the tunnel | #244; not a deployment setting |
| WireGuard endpoint | No default; a DNS name recommended | `WIREGUARD_ENDPOINT` above |
| Snapshots | Local | Not a setting yet: local is all there is. Off-cluster S3 is #200's, which adds its settings here |
| DHCP, per `Network` | `off` | The fleet config repo's `Network` documents (`docs/Config Schema.md`), not deployment config |
| Microsoft UEFI CA in enrolled Secure Boot keys | Enrolled | `bootstrap gen-secureboot-keys --no-microsoft-ca`; not a web app setting |
| Web app keys: WireGuard identity and store key (§32) | Operator-supplied identity (`secrets/wireguard.key`), operator-held store key | TPM sealing is #509's, whose design is open; it names its own setting when it lands |
| step-ca root (§32) | Offline; only an intermediate online | #506 names its settings |
| Browser UI on a LAN host listener (§32) | Off; the UI is on the tunnel listener only | #508 names its settings |
| Network addressing in git (§28) | Rejected: the repo is assumed public | `CONFIG_REPO_PRIVATE` above |
| Where state addressing comes from (§28) | Unset: no state addressing | `NETWORK_ADDRESSING_PATH` above; later the encrypted store (#514) |

Settings that later issues add (#197's identity, #218's git credentials, #397's
operator cert list and TLS) each become a row in `deployconfig.Settings` and in
[§ Settings](#settings), or a file under `secrets/`.
