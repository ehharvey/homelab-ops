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
  multi-GB, and the appliance's seed carries the directory inline in its
  NoCloud `user-data` (`bootstrap render-appliance-seed`, [§ The
  appliance](#the-appliance)).

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
- `bootstrap render-appliance-seed --no-secrets` (#243) leaves out exactly
  `secrets/`. Without it, the seed's `user-data` holds `secrets/` in
  plaintext, so the seed stick is secret until it's wiped.

## The appliance

The default host form (§29, #243): a NixOS box that runs
[`deploy/compose.yaml`](https://github.com/ehharvey/homelab-ops/blob/main/deploy/compose.yaml)
under Docker, from this directory. One generic image serves every operator.
It is built from the flake in
[`deploy/nixos/`](https://github.com/ehharvey/homelab-ops/tree/main/deploy/nixos)
and carries no operator settings. Each operator's settings, and this
directory, arrive in a NoCloud seed at first boot. The LAN laptop is the
bootstrap and break-glass appliance (§31). The internet VM form is installed
with nixos-anywhere instead (#569).

On the appliance:

```text
/etc/homelab-ops/
├── compose.yaml               deploy/compose.yaml, the web image pinned by digest
├── web/                       this directory (DEPLOYMENT_CONFIG_DIR), mounted read-only
├── appliance/                 the box's local flake: what nixos-rebuild builds
│   ├── flake.nix              generated, rewritten at every boot
│   ├── flake.lock             the versions the last OS upgrade moved to
│   └── settings.json          the host settings below
└── settings.applied.json      the settings the running system was built with
```

The store lives on the compose file's `store` volume (`STORE_PATH` points at
it), and every published port but WireGuard's `51820/udp` stays closed, so
`web.env` must leave `WIREGUARD_PORT` at its default. The web service
(`homelab-ops-web.service`) waits for `web/secrets/` before it starts. After
that Docker restarts the container itself (`restart: unless-stopped`).

**No shell by default.** Without `--ssh-authorized-keys` the appliance has
no shell at all: no sshd, no other user, and root has no password (NixOS
locks it), so not even the console logs in. `render-appliance-seed` notes
this on stderr. Nothing in this runbook needs one: you confirm an install by
the web app answering over the tunnel, roll back from the boot menu, and
change anything by reinstalling with a new seed. The few steps that need a
shell say so; they need a seed rendered with SSH keys.

### The seed

`bootstrap render-appliance-seed` writes `user-data` and `meta-data`, mode
`0600`:

| Flag | Default | What it does |
| --- | --- | --- |
| `--config-dir <dir>` | required | Every file of this directory goes inline in `user-data` with its mode, except dot entries (`.git/`, `.DS_Store`) and editor backups (`name~`), which it names and leaves out. What the seed carries is checked, and only that, with `deployconfig`'s own startup checks, run with `deploy/compose.yaml`'s environment, so one the web app would refuse is refused here; `secrets/` must be `0700` and its files `0600` or stricter. The web container mounts only this directory (and the store volume), so `NETWORK_ADDRESSING_PATH` and `BASE_IMAGE_PATH` must name a file the seed carries, as `/etc/homelab-ops/web/<file>`. The appliance serves the API only through the tunnel, so `WIREGUARD_ENDPOINT` is required. The seed empties `/etc/homelab-ops/web` and creates `secrets/` with its mode before writing into it. |
| `--no-secrets` | off | Leaves out exactly `secrets/`. The web app starts once it's copied in over SSH (below), so this needs `--ssh-authorized-keys`. Without `--no-secrets`, a directory with no `secrets/` is refused. Either way, the appliance would wait for it forever. |
| `--image <ref>` | `deploy/compose.yaml`'s digest | The web image, pinned by digest: `<repo>@sha256:<hex>`, or a bare `sha256:<hex>` for `ghcr.io/ehharvey/homelab-ops/web`. A tag is refused. |
| `--reboot-time HH:MM` | `04:00` | When the daily OS upgrade runs, in the host's local time (UTC unless you set a time zone with `timedatectl`), and the start of the one-hour window in which it may reboot. |
| `--ssh-authorized-keys <file>` | none: SSH off, no shell | An `authorized_keys` file: one bare public key per line, no options. With keys, sshd runs and port 22 opens, and root logs in with those keys only, never a password. Without, the appliance has no shell (above). |
| `--output <dir>` | `./bootstrap-output/appliance-seed` | Where the two files go. `--force` overwrites them. |

**How the settings take effect without rebuilding the image.** The seed
writes `settings.json` into the box's local flake. That flake builds this
repo's appliance with those settings. `homelab-appliance-settings.service`
compares `settings.json` with the settings the running system was built
with. When they differ, at the first boot or after you edit the file (over
SSH; without a shell, reinstall with a new seed), it runs
`nixos-rebuild switch` on the local flake against the running system's own
sources, so applying settings never changes the version. It needs the
network, to fetch the few changed packages from `cache.nixos.org`, and retries
every minute until it succeeds. Until it finishes, the image's defaults hold:
the `04:00` reboot time, and SSH off. A seed rendered at the defaults (no
`--reboot-time`, no keys) matches the image, so it rebuilds nothing. It
takes turns with the daily OS upgrade (they share a lock), and leaves a
newer generation that waits for its reboot alone, rather than switch back
to the running one's sources: the settings then apply when that generation
boots, or with the next upgrade, which builds from `settings.json` too.

### Install a laptop

1. **Build or download the image.** `make appliance-image` builds
   `bootstrap-output/appliance/homelab-ops-appliance.img` from
   `deploy/nixos/flake.lock`, in a pinned `nixos/nix` container. Without KVM
   it is slow, but it needs only Docker.
2. **Write it to the laptop's internal disk**, from another machine or a live
   USB: `dd if=homelab-ops-appliance.img of=/dev/<disk> bs=4M conv=fsync`. At
   the first boot the root partition grows to fill the disk.
3. **Make the seed stick.** Format a USB stick as FAT and label it `CIDATA`
   (`mkfs.vfat -n CIDATA /dev/<stick>`), then copy `user-data` and
   `meta-data` onto it.
4. **Boot the laptop with the stick in**, on a wired NIC: the image has no
   Wi-Fi setup. Disable Secure Boot in the firmware, because the image isn't
   signed (below). cloud-init writes the seed's files, the settings are
   applied, the web image is pulled, and the web app starts.
   **Confirm it by the web app answering:** from an operator device listed
   in `operator-peers`, `bootstrap api status` (its tunnel flags are in
   `bootstrap api --help`) reaches the API over the tunnel. Give the first
   boot a few minutes to pull the web image. With an SSH-enabled seed you
   can also watch the boot itself: `cloud-init status --wait` prints
   `status: done`.
5. **Remove the stick, and wipe it**, unless the seed was rendered with
   `--no-secrets`: it holds `secrets/` in plaintext. Later boots don't read
   it: once the first boot is done, the appliance shreds cloud-init's cached
   copy of `user-data`, so the key's only copy on the box is in `secrets/`,
   and turns cloud-init off for good.

**Changing anything** (a setting, the config directory, the web image)
without a shell means reinstalling: render a new seed and repeat steps 2 to
5. That rewrites the whole disk, the store volume with it, so the web app
starts with an empty store, as on a new host; carrying the store over is
§29's migration path (#245).

**Re-seeding, with an SSH-enabled seed.** Over SSH, remove
`/var/lib/homelab-ops/seed-consumed`, run `cloud-init clean`, and reboot
with the new stick in. The new seed replaces `/etc/homelab-ops/web` whole:
its first step empties the directory, so a file you dropped from
`--config-dir` (`operator-peers`, to revoke every operator peer, or an old
`client.crt`) doesn't linger. `compose.yaml` and `settings.json` are
rewritten, and the store volume is untouched. Docker waits for the seed's
files before it restarts the web container, so the container starts from
the new ones. A `--no-secrets` re-seed empties `secrets/` too: copy it in
again (below).

The laptop never sleeps: the image ignores the lid switch and the suspend and
hibernate keys, and the sleep targets are disabled, so it can sit closed on a
shelf.

**Disk size.** The image is about 7 GB. Each OS upgrade adds a generation
beside the running one, until the weekly garbage collection drops those over
30 days old, so plan on about 25 GB for the OS. The web image and the store
take little. With server-side image builds
on (`BASE_IMAGE_PATH`), add the base image, about 3.2 GB for IncusOS, plus
one image's size per concurrent download, since each build writes a copy.

**With `--no-secrets`.** The web service waits until
`/etc/homelab-ops/web/secrets/` exists. Copy the directory to a staging name
first and move it into place, so it appears complete: for example, with SSH
keys in the seed,

    scp -rp secrets root@<appliance>:/etc/homelab-ops/web/secrets.new
    ssh root@<appliance> mv /etc/homelab-ops/web/secrets.new /etc/homelab-ops/web/secrets

Keep the modes: `0700` for the directory, `0600` for its files.

### Upgrades and rollback

- **The OS upgrades itself.** `nixos-upgrade.service` runs daily at the
  reboot time. It runs:

      nixos-rebuild boot --refresh --flake /etc/homelab-ops/appliance#appliance \
        --update-input appliance --update-input appliance/nixpkgs

  `--update-input appliance` follows this repo's `main`.
  `--update-input appliance/nixpkgs` re-resolves the nested nixpkgs input to
  the head of the release branch `main`'s `flake.nix` names (today
  `nixos-26.05`), rather than the commit `main`'s `flake.lock` pins. So a
  release's security patches arrive daily without a repo PR. Moving to a new
  NixOS release is a repo PR that changes the branch (#570). If the kernel,
  initrd or kernel modules changed, the box reboots inside the one-hour window
  from the reboot time. Otherwise it switches in place. `/etc/homelab-ops/appliance/flake.lock`
  records what it moved to.
- **The web image upgrades only when you upgrade it.** It stays pinned by
  digest, and a running appliance never reads a seed again, so re-rendering
  alone changes nothing. Render the seed with a new `--image` and reinstall
  (above), or re-seed over SSH. With an SSH-enabled seed you can instead
  edit the `image:` line of `/etc/homelab-ops/compose.yaml` on the appliance
  and run `docker compose pull && docker compose up -d` in
  `/etc/homelab-ops/`; the store volume carries over.
- **Rolling back a bad OS generation.** Every generation stays in the
  systemd-boot menu, up to the last ten, which shows for a few seconds at
  every boot. Reboot (power-cycle, without a shell) and pick the previous
  one. That holds for that boot: the next one starts the newest generation
  again, so pick the previous one at each boot until a fixed upgrade lands.
  With an SSH-enabled seed, `nixos-rebuild switch --rollback` makes it the
  default instead. Either way the next daily upgrade moves forward again, so
  fix or pin the cause first.

### After a reboot

Once #197 lands, the web app's store is sealed after every restart, so each
reboot needs the operator's `unlock` before the web app serves again. §27
accepts the downtime, since nothing in the cluster depends on the web app,
and the appliance reboots only when an OS upgrade requires it. In TPM mode
(#509) the box unseals itself instead, and that issue's PCR choice decides
whether a kernel or firmware update forces the recovery key.

Out of scope here: Secure Boot for the appliance's own boot. §31 covers the
appliance signing node images, not how it boots itself.

## Every §29 setting

§29's operator-choice table, row by row. The rows that are `cmd/web` settings
point at the table above; the rest are chosen somewhere else and get no env
var here.

| §29 row | Default | Where it is set |
| --- | --- | --- |
| Host form | The NixOS appliance: the generic image (`make appliance-image`) plus a NoCloud seed | `bootstrap render-appliance-seed`, which pins the web image in `deploy/compose.yaml` with `--image` (default: the file's committed digest); not a web app setting. [§ The appliance](#the-appliance) |
| Host OS updates | `system.autoUpgrade` daily from `main`, rebooting only when the kernel, initrd or modules changed, in the hour from the reboot time | `render-appliance-seed --reboot-time HH:MM`, the host's local time (default `04:00`) |
| How the config directory reaches the host | Inline in the seed's `user-data`, the WireGuard key included | `render-appliance-seed --config-dir <dir>`: inline by default, or `--no-secrets`, which leaves out `secrets/` for the operator to copy in after boot |
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
