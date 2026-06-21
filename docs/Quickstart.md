# Quickstart

Getting node #0 up and running with the bootstrap CLI, end to end.

## 1. Open the devcontainer

VS Code → "Reopen in Container". This automatically:
- Configures an `incus` remote (`homelab-host`) trusting your dev container.
- Simulates the target network (`home-lan`) locally, so the pipeline below
  works without real hardware.

## 2. Build, test, lint

    make build && make test && make lint

Should pass clean on a fresh checkout. See [Development Conventions](Development%20Conventions)
for the full `make` target list.

## 3. Run the bootstrap pipeline

You need a `fleet.yaml` with exactly one `kind: Network` and one
`kind: Instance` document — see [Architecture](Architecture) § Data model for the
shape.

    ./bin/bootstrap gen-cert --output-dir ./bootstrap-output/cert

Generates a self-signed ECDSA P-384 client cert/key pair entirely offline.
This becomes the credential used later to authenticate against node #0's
Incus API. The key (`client.key`) is `0600` and gitignored — never commit
it.

    ./bin/bootstrap render-seed --file fleet.yaml --cert ./bootstrap-output/cert/client.crt --output-dir ./bootstrap-output/seed

Renders the four IncusOS install seed files — `install.yaml` (disk target,
TPM/Secure Boot flags), `network.yaml` (static IP or DHCP, plus a default
route), `applications.yaml` (`incus` only), `incus.yaml` (preseeds Incus to
trust `--cert` as a client cert, so it's trusted on first boot — run
`gen-cert` before `render-seed`, or cert trust never gets configured).

    ./bin/bootstrap build-image --image ./incusos-base.img --output ./bootstrap-output/img/node0.img

Copies a base IncusOS raw image and shells out to the upstream
`flasher-tool` binary (`go install github.com/lxc/incus-os/incus-osd/cmd/flasher-tool`,
must be on `$PATH`) to inject the seed into the copy. The base image itself
is never modified. Result is a `.img` ready to `dd` onto a USB stick — or
run `scripts/validate-issue-5.sh` to boot it in a real Incus VM and confirm
install + cert trust end-to-end without real hardware.

Run any subcommand with `--help` for the full flag list.

## What's next

- [Architecture](Architecture) — what this app is and how the pieces fit
- [Roadmap](Roadmap) — current phase/status
- [Development Conventions](Development%20Conventions) — branching, PR format, Go layout, vendoring rules
- [Open Questions](Open%20Questions) — resolved design decisions with rationale
