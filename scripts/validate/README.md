# The validation suite

These scripts each drive a real pipeline end-to-end, proving a "done when"
criterion `make test` can't. See `CLAUDE.md` § Validating changes for real for
why they exist, and `docs/Decisions.md` §20 for why they're bash rather than Go.

Each is named for the **behaviour it proves**, not the issue that prompted it —
an issue number ages into meaninglessness. The originating issue is recorded in
each file's header comment and in its `VALIDATE_PROVES` declaration.

## Running them

```
make validate                     # the unattended subset — CI's intended entry point
make validate-hardware            # needs the Incus remote and a real VM boot
make incusos-base                 # once per host: fetch the IncusOS base image they use

./scripts/validate/run.sh --group compose
./scripts/validate/run.sh --describe            # the prerequisite matrix, live
./scripts/validate/run.sh --list --group compose --json
```

Any script also runs standalone and takes the same flags:

```
./scripts/validate/image-route-streams-seeded-image.sh --describe
./scripts/validate/image-route-streams-seeded-image.sh --strict --allow-skip base-image
```

## Exit codes

| code | meaning |
|---|---|
| `0` | every check ran and passed |
| `1` | at least one check **failed** — a real defect |
| `2` | **hard** prerequisites unmet; declined to run at all |
| `3` | no failures, but at least one check was **skipped** |

The `2`/`3` split is the point of the harness. Before it, "you didn't install a
tool" and "the thing under test is broken" were the same outcome — #136 is what
that cost: a missing `go install` presented as five failures that read like a
node-provisioning regression.

`run.sh` aggregates: any `1` → `1`; else any `2` → `2`; else any `3` → `3`; else
`0`. "Something is broken" always outranks "something couldn't run", which
always outranks "everything that ran, passed".

## Skips, tags, and `--strict`

An unmet *soft* prerequisite produces a `SKIP` carrying a reason tag:

```
SKIP: image route returns 200 [base-image] — INCUSOS_BASE_IMAGE unset and the pinned IncusOS image is not cached — run 'make incusos-base'
```

Every `[base-image]` skip carries that reason (`base_image_skip_reason` in
`lib.sh`), so the output names the command that makes the check run.

`--strict` turns any skip into a failure, except those explicitly allowed:

```
run.sh --group compose --strict --allow-skip base-image
```

That combination is the suite's intended anti-rot mechanism. A hosted runner
has no 3.2 GB IncusOS image and no workflow fetches one (see
[The IncusOS base image](#the-incusos-base-image-296)), so `base-image` skips
are blessed — but **any other** skip fails the build, including a *new* tag nobody
has blessed yet. A route that silently gains a precondition produces a `FAIL`,
not a quietly-tolerated skip, on the day it lands rather than weeks later by
accident (which is exactly how #107 broke four scripts).

**Nothing runs this automatically yet.** There is no validate workflow in
`.github/workflows/`; the mechanism above is built and ready, but until it is
wired up it only protects the runs someone remembers to start by hand. Wiring
it is tracked separately, and the "runs in CI" column below states intent, not
current fact.

Tags in use: `base-image`, `ct-image`, `flasher-tool`, `incus-instances`,
`incus-socket`, `upstream`. Four describe a prerequisite the environment
didn't supply — an IncusOS image, a container image, the `flasher-tool`
binary, or a readable/writable Incus unix socket. `incus-instances` is a
*probe* rather than a named cause: a script that needs to create instances
asks the daemon to create one and skips with the daemon's own words if it
can't, which covers an uninitialized daemon (no storage pool) and a
devcontainer with no idmap without enumerating either. `upstream` means "a
prior check already failed, so this couldn't run" — a cascade, not a missing
prerequisite.

Enumerate them from the scripts rather than trusting this list:

```
grep -rhoE 'skip_check "[^"]*" [a-z-]+' scripts/validate/*.sh | awk '{print $NF}' | sort -u
```

## The IncusOS base image (#296)

The `[base-image]` checks need a real IncusOS raw image: 3.2 GB, gitignored,
and not something the repo can carry. One command supplies it:

```
make incusos-base     # scripts/fetch-incusos-base.sh; ~610 MB down, 3.2 GB on disk
```

It fetches the version pinned in `scripts/incusos-base.version` from the Linux
Containers CDN, verifies it, caches it, and prints an
`export INCUSOS_BASE_IMAGE=…` line on stdout. A second run downloads nothing.
After it, the validate scripts find the image themselves.

**Trust chain.** The CDN publishes no `.sha256` file. The checksum is in
`<version>/update.sjson`, an S/MIME-signed manifest, so the fetch is:

1. `scripts/incusos-keys/root-E1.crt`, the committed trust root (`CN = Incus OS
   - Root E1`, SHA-256 fingerprint `10:D0:90:00:12:CF:8E:60:E0:23:B4:C2:96:32:57:63:59:63:9F:F9:F2:53:24:68:4C:74:4C:8A:0C:EC:8D:47`,
   expires 2045-06-21). It was downloaded once and checked against that
   fingerprint. It is never fetched at run time: taking the trust root from the
   CDN being verified would be circular.
2. `update.sjson` must verify against that root (`openssl smime -verify`, with
   the system CA store excluded), **and** its signer must have been issued by
   the `Incus OS - Update E1` CA, itself issued directly by Root E1. Root E1
   alone would accept a signer under any of its intermediates; upstream trusts
   only the Update CA (`incus-osd/internal/providers/provider_images.go`). The
   signing certificates themselves rotate yearly (`Incus OS - Update 2025 E1`,
   `… 2026 E1`), which is why the check is on their issuer.
3. The `.img.gz`'s sha256 is read from that verified JSON and must match the
   download.
4. The decompressed `.img`'s sha256 goes in a sidecar, `IncusOS_<v>.img.sha256`.
   Every later `make incusos-base` re-checks the image against it. This catches
   a truncated or corrupted cache; it says nothing new about authenticity.

Any failure exits non-zero and leaves nothing at the final path. The image's
own contents (sysext and verity signatures) are not verified: the chain stops
at the signed manifest's sha256.

**Cache layout.** One directory per version, under the *main checkout*:

```
<main checkout>/bootstrap-output/incusos/<version>/IncusOS_<version>.img
<main checkout>/bootstrap-output/incusos/<version>/IncusOS_<version>.img.sha256
```

It is shared by every worktree on purpose. Each issue gets its own worktree
(#252) with its own empty `bootstrap-output/`, so a per-checkout cache would
leave every worker skipping. `make incusos-base` run from any worktree fills
the same directory, and a later run from another is a no-op printing the same
path. The directory is gitignored, so this never touches tracked work. A fetch
is assembled in a temp directory beside the cache and renamed into place whole,
so an interrupted run leaves nothing behind that a later check would trust. (A
run killed outright can leave that `.fetch.*` directory; it is safe to delete.)

If a cached image stops matching its sidecar, the script exits non-zero and
names the directory to delete. It never re-fetches silently.

A flat, hand-placed `bootstrap-output/incusos/IncusOS_<version>.img` from
before this existed is neither used nor touched. Delete it once
`make incusos-base` has run.

**The default is the pinned version only.** When `INCUSOS_BASE_IMAGE` is unset,
`default_incusos_base_image` (`lib.sh`) reads *this checkout's*
`scripts/incusos-base.version` and uses that version's cached image if it is
there, printing one `NOTE` with the path and version. It does not fall back to
some other cached version. After a pin bump, or when only another version was
fetched, the checks skip until `make incusos-base` has run again. That keeps
two machines on the same commit from quietly testing different IncusOS builds,
the drift `docs/Decisions.md` §21 was written against. The harness never
fetches (a 610 MB download shouldn't hide inside a test run) and never
re-hashes the image.

To test against another image, say so explicitly:

```
INCUSOS_VERSION=202609271243 make incusos-base   # fetch a non-pinned version
export INCUSOS_BASE_IMAGE=…                      # the line it printed
```

`INCUSOS_VERSION` overrides the pin for the fetch script only; the harness
ignores it.

**The pin and how it moves.** `scripts/incusos-base.version` is one line: a
CDN `stable`-channel version. It is *not* the `third_party/incus-os` submodule
tag. Not every tag gets a CDN build, and old builds are pruned, so the two move
independently.

`.github/workflows/bump-incusos.yml` runs daily. Its `bump-base-pin` job asks
`scripts/fetch-incusos-base.sh --latest-stable` for the newest `stable` version
in the verified `os/index.sjson`, and when that differs from the pin it opens a
PR on `chore/bump-incusos-base` changing only the pin file. That PR is separate
from the submodule bump's `chore/bump-incus-os`, and is opened whether or not
lxc/incus-os cut a new tag.

**Pin PRs need an operator push before their checks run.** The PR is opened
with `GITHUB_TOKEN`, and a `GITHUB_TOKEN` push triggers no workflows. So it
arrives without the required checks (`build-test`, `lint`, `docker-smoke`,
`one-commit`) and stays blocked. To land one: check out the branch, run
`make incusos-base` and the scripts that use the image, then push to the branch
(amending the commit is enough). This is a known limitation and it is accepted:
a pin refresh is meant to be a conscious act (§21), not something that merges
itself.

The fetch script has three environment overrides for testing its failure paths
(`INCUSOS_CDN_URL`, `INCUSOS_ROOT_CERT`, `INCUSOS_CACHE_DIR`). Each is announced
on stderr when set, and none of them skips a verification step. The script's
header documents them.

## Prerequisite matrix

Don't maintain a table here — it would drift the moment a script changed, which
is the disease this suite exists to treat. Ask the scripts instead:

```
./scripts/validate/run.sh --describe
```

Each declares `VALIDATE_PROVES`, `VALIDATE_GROUP`, `VALIDATE_NEEDS` and
`VALIDATE_DURATION`; `--describe` prints them and exits without running
anything. Square brackets in `needs` mark an *optional* prerequisite whose
absence causes a skip rather than a failure.

## Groups

| group | what it needs | belongs in CI |
|---|---|---|
| `none` | Go only | yes |
| `compose` | Docker, `docker compose` | yes |
| `incus` | an Incus remote **or** a readable/writable Incus unix socket, `jq` | no — needs the host |
| `incus-vm` | an Incus remote, the pinned base images, a real VM boot, the IncusOS base image (`make incusos-base`, or `INCUSOS_BASE_IMAGE`) | no — needs the host |
| `github` | authenticated `gh` with repo admin | no — opens real PRs, would recurse |

`run.sh` derives these from the scripts themselves rather than a list kept here,
so a new script is covered without editing anything central.

Target the Incus groups elsewhere with `VALIDATE_INCUS_REMOTE`,
`VALIDATE_INCUS_PROJECT`, `VALIDATE_INCUS_NETWORK` (#132) — needed to run on the
Incus host itself, where Incus is a local unix socket and no `homelab-host`
remote exists. `VALIDATE_INCUS_POOL` and `VALIDATE_ALPINE_CT` /
`VALIDATE_ALPINE_VM` (#131) override the storage pool and the pinned base image
aliases the same way.

`run.sh --group incus` is a one-second health check on the host and the client
— it asserts the remote is reachable, the network exists, and that the Incus
client and server share a major version.

## Parallelism

`run.sh --jobs N` runs scripts concurrently. **The `compose` group cannot use it
yet**: those scripts share host ports 8080/3000/3100/9090 and a single Compose
project name, so two at once fight over both. The `incus-vm` group likewise
shares the `home-lan` bridge, and `app-produces-working-installer-e2e.sh`'s
header warns against running it concurrently with
`node-boots-and-trusts-bootstrap-cert.sh` for that reason.

Per-run isolation is tracked separately. Until then `--jobs` is safe only across
groups that don't contend — and the measured serial cost is low: the whole
non-hardware set runs in about two and a half minutes.

Serially, the same contention can bite at the boundary between two compose
scripts: one script's teardown must fully release the shared ports and project
network before the next script's `compose up`. That is what `compose_down`
(in `lib-compose.sh`) guarantees — every compose script's cleanup trap calls it
instead of a bare `docker compose down`, and it blocks until the containers and
published ports are actually gone. On Compose v5 `down` is already synchronous,
so the barrier is a no-op there; it exists so the group stays deterministic on
an older or CI-provided Compose where `down` returns early (#153).

## The library

| file | contents | sourced by |
|---|---|---|
| `lib.sh` | counters, `check`/`check_json`/`check_eq`, `skip_check`, the prereq DSL, `default_incusos_base_image`/`base_image_skip_reason`, arg parsing, `summary` | all |
| `lib-compose.sh` | `compose()`, `compose_down` (teardown barrier, #153), `wait_web_ready`, `wait_http`/`wait_json`, `check_log`/`wait_log`, `push_fleet` | `compose` |
| `lib-incus.sh` | `console_log`, `wait_for_console_text`, `incus_exec_bg`, `require_flasher_tool`, the pinned-image aliases, `incus_versions_compatible` | `incus`, `incus-vm` |

`lib.sh`'s prereq DSL includes `require_incus_image`, which asserts the pinned
base images exist. It is a hard prerequisite (exit 2), not a skip: a missing
alias means operator setup wasn't run, the same class as "the remote isn't
there" — not an environmental limit like a missing multi-gigabyte IncusOS
image, which is a skip (below). The diagnostic names the script that fixes it.

Two deliberate irregularities, both documented in the scripts themselves:

- **The `github` scripts are fail-fast** (`multi-commit-pr-cannot-reach-main.sh`,
  `amended-commit-reships-to-open-pr.sh`). Each drives one real PR through
  GitHub and each step depends on the last, so there is nothing to accumulate
  after a failure. They use the shared recorders for output and exit codes but
  keep their own aborting `fail`.
- **`cmd/validate-tunnel-harness` is a Go program.** It runs *inside* a container
  to drive create-instance from the node's side of the tunnel, which bash can't
  do from outside. It's a fixture the bash drives — the same category as
  `bootstrap` or `flasher-tool` — not a test. Go's layout requires it live under
  `cmd/`, so the suite is all-bash with one Go fixture binary.

## What the scripts name things on the Incus host (#131)

Every object a script creates on the host carries a recognisable prefix, so
anything left behind can be identified and removed by hand:

| object | pattern | example |
|---|---|---|
| instances | `validate-<slug>-<role>-$$` | `validate-tunnel-gw-31337` |
| custom volumes | `validate-<slug>-<purpose>-$$` | `validate-nodeboot-seeded-img-31337` |
| networks | `valwan-$$` | `valwan-31337` |

Networks break the pattern on purpose: a bridge's name becomes a real
host-level interface name (`ip link`), which is capped at 15 characters.
`validate-wan-` plus a PID does not fit.

**What this buys, and what it doesn't.** `validate-*` is a safe glob for
spotting leftovers, but `$$` is the *devcontainer's* PID and means nothing on
the host — so a re-run cannot recognise or reclaim its own previous leftovers,
and two concurrent runs cannot tell each other's objects apart. Cleanup is
`trap ... EXIT` only, so `kill -9`, a devcontainer rebuild mid-run, or a host
reboot orphans everything that run created.

A reaper to collect leftovers automatically was considered and declined (#131):
the convention makes them identifiable by hand, and in practice the suite has
not been observed to leak. Worth revisiting if that stops being true — Incus
records `created_at` per instance, so age-based collection over the
`validate-*` glob is the obvious shape.
