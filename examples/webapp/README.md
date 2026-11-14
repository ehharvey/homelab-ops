# Example deployment config directory

The web app's deployment config (`DEPLOYMENT_CONFIG_DIR`), laid out as
[`docs/Deployment.md`](../../docs/Deployment.md) describes. It's the default,
tunnel-only posture: the API is served only inside the WireGuard tunnel, to
the operator peers listed in `operator-peers`.

To use it, copy the directory to the appliance (for example
`/etc/homelab-ops/web/`), then:

1. In `web.env`, set `WIREGUARD_ENDPOINT` to the DNS name nodes will dial and
   `CONFIG_REPO_URL` to your fleet config repo.
2. In `operator-peers`, add a line for each operator device's public key.
3. Add `client.crt`, the public half of `bootstrap gen-cert`'s break-glass
   cert. Never its private key.
4. Create `secrets/` with mode `0700`, and in it the web app's WireGuard
   identity, `secrets/wireguard.key` (mode `0600`): `wg genkey >
   secrets/wireguard.key`. It's required while `WIREGUARD_ENDPOINT` is set.
   Back it up: its public half is baked into every node's seed.
5. Mount the directory read-only and set `DEPLOYMENT_CONFIG_DIR` to it.
6. Once it's running, the store is locked: run `bootstrap api init
   --key-file <file>` once, and `bootstrap api unlock --key-file <file>`
   after every restart. Keep that key file safe and off the appliance.

Nothing secret is committed here: git can't record `secrets/`'s modes, and an
example key would be a real key anyone could read. This README is ignored by
the web app, as is any other file it doesn't name.

`scripts/validate/web-starts-from-deployment-config-directory.sh` starts the
real compose stack from a copy of this directory. It fills in the operator
peer, the cert and the identity it generates, overrides
`WIREGUARD_ENDPOINT` from the environment, and runs init over the tunnel.
