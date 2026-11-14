# Example deployment config directory

The web app's deployment config (`DEPLOYMENT_CONFIG_DIR`), laid out as
[`docs/Deployment.md`](../../docs/Deployment.md) describes. It's the default,
tunnel-only posture: the API is served only inside the WireGuard tunnel, to
the operator peers listed in `operator-peers`, over TLS, to clients presenting
an operator cert (#397).

To use it, copy the directory to the appliance (for example
`/etc/homelab-ops/web/`), then:

1. In `web.env`, set `WIREGUARD_ENDPOINT` to the DNS name nodes will dial and
   `CONFIG_REPO_URL` to your fleet config repo.
2. In `operator-peers`, add a line for each operator device's public key.
3. Add `client.crt`, the public half of `bootstrap gen-cert`'s break-glass
   cert. Never its private key. The API requires a client to present it
   (#397), so `bootstrap api` passes it with `--client-cert`/`--client-key`.
   For a second operator device with its own cert, add `operator-certs.pem`
   instead: every operator cert's public half, concatenated. Unset, the list
   is `client.crt` alone. As committed, with no operator peer and no cert,
   the directory loads and the API refuses everyone; once step 2 lists a
   peer, startup requires this step too.
4. Create `secrets/` with mode `0700`, holding the web app's WireGuard
   identity as `secrets/wireguard.key` (mode `0600`, `wg genkey` output).
   #197 will load it for the tunnel; until then, with the tunnel on, the
   tunnel's identity, and the API's TLS key derived from it, come from the
   store, and this file isn't read (startup warns if it differs). With the
   tunnel off and a TLS host listener on, the TLS key is derived from this
   file, which startup then requires. So switching the tunnel on or off
   changes the TLS key's fingerprint until #197.
5. Mount the directory read-only and set `DEPLOYMENT_CONFIG_DIR` to it.

Nothing secret is committed here: git can't record `secrets/`'s modes, and an
example key would be a real key anyone could read. This README is ignored by
the web app, as is any other file it doesn't name.

`scripts/validate/web-starts-from-deployment-config-directory.sh` starts the
real compose stack from a copy of this directory. It fills in the operator
peer, the cert and the identity it generates, and overrides
`WIREGUARD_ENDPOINT` from the environment. The web app logs its TLS key's
fingerprint at startup (`TLS server key fingerprint sha256//…`); that is what
`bootstrap api` asks you to confirm on first use.
