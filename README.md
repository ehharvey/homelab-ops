# homelab-ops

Code for the homelab-ops project. Design docs (Architecture, Roadmap, Open
Questions) live in the [project wiki](https://github.com/ehharvey/homelab-ops/wiki),
cloned automatically into a sibling `../wiki` directory by the devcontainer
(`.devcontainer/scripts/0-clone-wiki.sh`).

## Bootstrap CLI

A standalone, offline-first CLI for getting node #0 up and running (see
wiki `Architecture.md` § "Bootstrap CLI"). Not part of the always-on web app.

    go build -o bin/bootstrap ./cmd/bootstrap
    ./bin/bootstrap gen-cert --output-dir ./bootstrap-output/cert

Generates a self-signed ECDSA P-384 client cert/key pair entirely offline.
The key (`client.key`) is written with `0600` permissions and is **not**
committed to git — see `.gitignore`. This becomes the credential used later
to authenticate against node #0's Incus API.

Run `./bin/bootstrap gen-cert --help` for all flags.

    ./bin/bootstrap render-seed --file fleet.yaml --output-dir ./bootstrap-output/seed

Reads a fleet definition YAML file (exactly one `kind: Network` and one
`kind: Instance` document) and renders the three IncusOS install seed files
— `install.yaml`, `network.yaml`, `applications.yaml`.

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

    make build   # build the bootstrap binary
    make test    # run unit tests
    make lint    # run golangci-lint
