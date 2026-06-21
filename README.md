# homelab-ops

Code for the homelab-ops project. Design docs (Architecture, Roadmap, Open
Questions) live under [`docs/`](docs/) in this repo — see
[`docs/Development Conventions.md`](docs/Development%20Conventions.md).
They're mirrored read-only to the
[project wiki](https://github.com/ehharvey/homelab-ops/wiki) for
browsability; never edit the wiki directly.

## Quickstart

1. Open this repo in the devcontainer (VS Code "Reopen in Container") — it
   configures an `incus` remote (`homelab-host`) trusting your dev
   container, and simulates the target network (`home-lan`) locally, so
   the full pipeline below works without real hardware.
2. `make build && make test && make lint` — should pass clean on a fresh
   checkout.
3. Run the bootstrap pipeline for node #0 (`fleet.yaml`: one `kind: Network`
   + one `kind: Instance` doc — see [`docs/Architecture.md`](docs/Architecture.md) § Data model):

       ./bin/bootstrap gen-cert --output-dir ./bootstrap-output/cert
       ./bin/bootstrap render-seed --file fleet.yaml --cert ./bootstrap-output/cert/client.crt --output-dir ./bootstrap-output/seed
       ./bin/bootstrap build-image --image ./incusos-base.img --output ./bootstrap-output/img/node0.img

   `dd` the result onto a USB stick, or run `scripts/validate-issue-5.sh` to
   boot it in a real Incus VM and confirm install + cert trust end-to-end
   without real hardware.
4. See [`docs/Quickstart.md`](docs/Quickstart.md) for the full walkthrough,
   or [`docs/Home.md`](docs/Home.md) for all the docs.

## Bootstrap CLI

A standalone, offline-first CLI for getting node #0 up and running (see
[`docs/Architecture.md`](docs/Architecture.md) § "Bootstrap CLI"). Not part of the always-on web app.

    go build -o bin/bootstrap ./cmd/bootstrap
    ./bin/bootstrap gen-cert --output-dir ./bootstrap-output/cert

Generates a self-signed ECDSA P-384 client cert/key pair entirely offline.
The key (`client.key`) is written with `0600` permissions and is **not**
committed to git — see `.gitignore`. This becomes the credential used later
to authenticate against node #0's Incus API.

Run `./bin/bootstrap gen-cert --help` for all flags.

    ./bin/bootstrap render-seed --file fleet.yaml --cert ./bootstrap-output/cert/client.crt --output-dir ./bootstrap-output/seed

Reads a fleet definition YAML file (exactly one `kind: Network` and one
`kind: Instance` document) and renders the four IncusOS install seed files
— `install.yaml`, `network.yaml`, `applications.yaml`, `incus.yaml`.
`incus.yaml` preseeds Incus to trust `--cert` (`gen-cert`'s output) as a
client certificate, so run `gen-cert` before `render-seed`. Without this,
Incus never trusts the bootstrap cert on first boot.

    ./bin/bootstrap build-image --image ./incusos-base.img --output ./bootstrap-output/img/node0.img

Copies a base IncusOS raw image to `--output` and shells out to the
upstream `flasher-tool` binary
(`go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool`, must be
on `$PATH`) to inject the seed from `--seed-dir` (default
`./bootstrap-output/seed`, i.e. `render-seed`'s output) into the copy in
place. The base image itself, obtained separately, is never modified.
Result is a `.img` ready to `dd` onto a USB stick.

Run `./bin/bootstrap build-image --help` for all flags.

## Development

    make build           # build the bootstrap binary
    make test            # run unit tests (-race -cover)
    make lint            # run golangci-lint (via Docker)
    make fmt             # gofmt + goimports
    make tidy            # go mod tidy
    make vendor-incusos  # regenerate internal/third_party/incusos after bumping the submodule
    make clean           # remove bin/ and bootstrap-output/

`scripts/validate-issue-N.sh` scripts each prove one GitHub issue's "done
when" criteria end-to-end against a real Incus remote/VM, not just unit
tests — run the relevant one after touching anything it covers.
