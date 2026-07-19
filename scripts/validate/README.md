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
SKIP: image route returns 200 [base-image] — INCUSOS_BASE_IMAGE not usable
```

`--strict` turns any skip into a failure, except those explicitly allowed:

```
run.sh --group compose --strict --allow-skip base-image
```

That combination is the suite's intended anti-rot mechanism. A hosted runner
genuinely cannot supply a 3.2 GB IncusOS image, so `base-image` skips are
blessed — but **any other** skip fails the build, including a *new* tag nobody
has blessed yet. A route that silently gains a precondition produces a `FAIL`,
not a quietly-tolerated skip, on the day it lands rather than weeks later by
accident (which is exactly how #107 broke four scripts).

**Nothing runs this automatically yet.** There is no validate workflow in
`.github/workflows/`; the mechanism above is built and ready, but until it is
wired up it only protects the runs someone remembers to start by hand. Wiring
it is tracked separately, and the "runs in CI" column below states intent, not
current fact.

Tags in use: `base-image`, `flasher-tool`, `incus`, `vm`, `github`, `upstream`
(the last meaning "a prior check already failed, so this couldn't run" — a
cascade, not a missing prerequisite).

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
| `incus` | an Incus remote, `jq` | no — needs the host |
| `incus-vm` | an Incus remote, the pinned base images, a real VM boot, `INCUSOS_BASE_IMAGE` | no — needs the host |
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
| `lib.sh` | counters, `check`/`check_json`/`check_eq`, `skip_check`, the prereq DSL, arg parsing, `summary` | all |
| `lib-compose.sh` | `compose()`, `compose_down` (teardown barrier, #153), `wait_web_ready`, `wait_http`/`wait_json`, `check_log`/`wait_log`, `push_fleet` | `compose` |
| `lib-incus.sh` | `console_log`, `wait_for_console_text`, `incus_exec_bg`, `require_flasher_tool`, the pinned-image aliases, `incus_versions_compatible` | `incus`, `incus-vm` |

`lib.sh`'s prereq DSL includes `require_incus_image`, which asserts the pinned
base images exist. It is a hard prerequisite (exit 2), not a skip: a missing
alias means operator setup wasn't run, the same class as "the remote isn't
there" — not an environmental limit like a missing multi-gigabyte
`INCUSOS_BASE_IMAGE`. The diagnostic names the script that fixes it.

Two deliberate irregularities, both documented in the scripts themselves:

- **`multi-commit-pr-cannot-reach-main.sh` is fail-fast.** It drives one real PR
  through GitHub and each step depends on the last, so there is nothing to
  accumulate after a failure. It uses the shared recorders for output and exit
  codes but keeps its own aborting `fail`.
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
