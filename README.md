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

## Development

    make build   # build the bootstrap binary
    make test    # run unit tests
    make lint    # run golangci-lint
