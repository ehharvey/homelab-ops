# The appliance's nixosTest (#243), run by `nix flake check` (make
# appliance-check). It boots the appliance module with a test seed on a
# CIDATA ISO, as a laptop boots with its seed stick, and asserts:
#
#   - cloud-init's cache of user-data is shredded after the first boot, and
#     cloud-init turned off;
#   - the seed's bootcmd empties /etc/homelab-ops/web before write_files, so
#     a file the seed doesn't carry doesn't linger;
#   - the web service waits while secrets/ is absent, and starts once it
#     arrives;
#   - the compose service runs from the pinned digest;
#   - `docker compose` (the CLI plugin the runbook uses) sees its project;
#   - only the WireGuard UDP port is open;
#   - the autoUpgrade reboot settings: daily at the reboot time, allowReboot,
#     the window from the seed's reboot time;
#   - nixos-upgrade and homelab-appliance-settings share one rebuild lock,
#     and a settings change defers to a boot default that isn't the running
#     system (an upgrade waiting for its reboot) rather than undo it.
#
# The test is offline, so it can't pull the real web image from GHCR. It
# stands a stub in for it: a tiny image that echoes UDP on 51820, built here
# as an OCI layout whose manifest digest is known at build time. The test
# pushes it, digest preserved, to a registry on the appliance's localhost, and
# the seed's compose.yaml (../compose.yaml with only its image line changed)
# pins it by that digest. The seed is written by hand here, in the shape
# `bootstrap render-appliance-seed` renders (internal/webappliance has the
# unit tests on that renderer); this flake can't build the Go CLI, which
# lives outside its source tree.
{
  pkgs,
  lib,
  applianceModules,
  composeFile,
}:
let
  settings = {
    rebootTime = "03:30";
    sshAuthorizedKeys = [ ];
  };

  stubImage = pkgs.dockerTools.buildLayeredImage {
    name = "homelab-ops-web-stub";
    tag = "test";
    config.Cmd = [
      "${pkgs.socat}/bin/socat"
      "UDP-LISTEN:51820,fork,reuseaddr"
      "PIPE"
    ];
  };

  # The stub as an OCI layout, and its manifest digest.
  stubOci =
    pkgs.runCommand "homelab-ops-web-stub-oci"
      {
        nativeBuildInputs = [
          pkgs.skopeo
          pkgs.jq
        ];
      }
      ''
        mkdir -p $out
        skopeo --insecure-policy --tmpdir "$TMPDIR" copy docker-archive:${stubImage} oci:$out/oci:web
        jq -r '.manifests[0].digest' $out/oci/index.json >$out/digest
        grep -qE '^sha256:[0-9a-f]{64}$' $out/digest
      '';

  # The seed: NoCloud user-data and meta-data on an ISO labelled cidata.
  seed =
    pkgs.runCommand "homelab-ops-appliance-test-seed"
      {
        nativeBuildInputs = [
          pkgs.cdrkit
          pkgs.gnused
        ];
      }
      ''
        digest=$(cat ${stubOci}/digest)
        ref="localhost:5000/homelab-ops/web@$digest"
        sed -E "s|^(    image: ).*|\1$ref|" ${composeFile} >compose.yaml
        grep -qF "image: $ref" compose.yaml
        cat >settings.json <<'EOF'
        ${builtins.toJSON settings}
        EOF
        printf '# test web.env\nWIREGUARD_ENDPOINT=appliance:51820\n' >web.env
        printf '# test operator-peers\n' >operator-peers

        mkdir -p iso
        {
          echo '#cloud-config'
          # What the scrub must leave nowhere under /var/lib/cloud.
          echo '# scrub-marker-homelab-appliance-test'
          echo 'bootcmd:'
          echo '  - [cloud-init-per, once, homelab-ops-clear-web, find, /etc/homelab-ops, -mindepth, "2", -path, /etc/homelab-ops/web/*, -delete]'
          echo 'write_files:'
          for f in compose.yaml:/etc/homelab-ops/compose.yaml \
                   settings.json:/etc/homelab-ops/appliance/settings.json \
                   web.env:/etc/homelab-ops/web/web.env \
                   operator-peers:/etc/homelab-ops/web/operator-peers; do
            echo "  - path: ''${f#*:}"
            echo '    owner: root:root'
            echo '    permissions: "0644"'
            echo '    encoding: b64'
            echo "    content: $(base64 -w0 "''${f%%:*}")"
          done
        } >iso/user-data
        printf 'instance-id: iid-homelab-ops-appliance-test\nlocal-hostname: homelab-appliance\n' >iso/meta-data
        mkdir -p $out
        genisoimage -volid cidata -joliet -rock -o $out/seed.iso iso
      '';
in
pkgs.testers.runNixOSTest {
  name = "homelab-ops-appliance";

  nodes.appliance = {
    imports = applianceModules settings;
    networking.hostName = lib.mkForce "appliance";
    # The test VM's disk is unpartitioned.
    boot.growPartition = lib.mkForce false;
    # A file left from an earlier seed, which the seed's bootcmd must clear.
    systemd.tmpfiles.rules = [ "f /etc/homelab-ops/web/stale 0644 root root - stale" ];
    virtualisation = {
      memorySize = 3072;
      diskSize = 8192;
      cores = 2;
      qemu.options = [
        "-cdrom"
        "${seed}/seed.iso"
      ];
    };
    services.dockerRegistry = {
      enable = true;
      listenAddress = "127.0.0.1";
      port = 5000;
    };
    environment.systemPackages = [ pkgs.skopeo ];
  };

  nodes.client = {
    environment.systemPackages = [
      pkgs.nmap
      pkgs.socat
    ];
  };

  testScript = ''
    import re

    digest = open("${stubOci}/digest").read().strip()
    ref = f"localhost:5000/homelab-ops/web@{digest}"

    start_all()
    appliance.wait_for_unit("cloud-final.service")
    status = appliance.succeed("cloud-init status --wait || true")
    assert "status: done" in status, status

    with subtest("cloud-init's cached user-data is shredded, and the seed marked consumed"):
        appliance.wait_until_succeeds("test -e /var/lib/homelab-ops/seed-consumed")
        appliance.succeed(
            "journalctl -u homelab-appliance-seed-scrub.service | grep -q 'shredded .*/user-data.txt$'"
        )
        appliance.fail("grep -rqF scrub-marker-homelab-appliance-test /var/lib/cloud")
        # cloud-init's units are off from the next boot on.
        appliance.succeed("systemctl cat cloud-init.service | grep -qx 'ConditionPathExists=!/var/lib/homelab-ops/seed-consumed'")

    with subtest("the seed's files are in place, and the web app waits for secrets/"):
        appliance.succeed("test -f /etc/homelab-ops/compose.yaml")
        appliance.succeed("grep -qF 'image: " + ref + "' /etc/homelab-ops/compose.yaml")
        appliance.succeed("test -f /etc/homelab-ops/web/web.env")
        appliance.fail("test -e /etc/homelab-ops/web/secrets")
        # bootcmd emptied web/ (the earlier seed's file is gone) and left
        # the rest of /etc/homelab-ops alone.
        appliance.fail("test -e /etc/homelab-ops/web/stale")
        appliance.succeed("test -f /etc/homelab-ops/settings.applied.json")
        appliance.wait_for_unit("docker.service")
        appliance.succeed("systemctl is-active homelab-ops-web.path")
        appliance.fail("systemctl is-active homelab-ops-web.service")
        appliance.succeed('test -z "$(docker ps -aq)"')

    with subtest("settings.json matches the built settings, so nothing is rebuilt"):
        appliance.wait_until_succeeds(
            "journalctl -u homelab-appliance-settings.service | grep -q 'is already applied'"
        )
        appliance.succeed("test -f /etc/homelab-ops/appliance/flake.nix")

    with subtest("the stub web image is published to the local registry, digest preserved"):
        appliance.wait_for_unit("docker-registry.service")
        appliance.wait_for_open_port(5000)
        appliance.succeed(
            "skopeo --insecure-policy copy --preserve-digests --dest-tls-verify=false "
            "oci:${stubOci}/oci:web docker://localhost:5000/homelab-ops/web:test"
        )

    with subtest("secrets/ arriving starts the compose service from the pinned digest"):
        appliance.succeed(
            "install -d -m 0700 /etc/homelab-ops/web/secrets"
            " && install -m 0600 /dev/null /etc/homelab-ops/web/secrets/wireguard.key"
        )
        appliance.wait_for_unit("homelab-ops-web.service")
        appliance.wait_until_succeeds(
            "docker inspect --format '{{.State.Running}}' homelab-ops-web-1 | grep -qx true",
            timeout=300,
        )
        image = appliance.succeed("docker inspect --format '{{.Config.Image}}' homelab-ops-web-1").strip()
        assert image == ref, f"container runs {image}, want {ref}"
        digests = appliance.succeed("docker image inspect --format '{{json .RepoDigests}}' " + ref)
        assert ref in digests, digests

    with subtest("the runbook's `docker compose` sees the service's project"):
        # docs/Deployment.md's web image upgrade: `docker compose pull &&
        # docker compose up -d` in /etc/homelab-ops, the CLI plugin form.
        print(appliance.succeed("docker compose version 2>&1"))
        names = appliance.succeed("cd /etc/homelab-ops && docker compose ps --format '{{.Name}}' 2>&1")
        assert "homelab-ops-web-1" in names.split(), names

    with subtest("only the WireGuard UDP port is open"):
        published = appliance.succeed("docker port homelab-ops-web-1").strip().splitlines()
        assert published and all(p.startswith("51820/udp ") for p in published), published
        tcp = appliance.succeed(
            "ss -Htln | awk '{print $4}' | grep -vE '^(127\\.[0-9.]+|\\[::1\\]|::1):' || true"
        ).strip()
        assert tcp == "", f"TCP listeners beyond loopback: {tcp}"
        appliance.fail("systemctl is-active sshd.service")
        client.wait_for_unit("multi-user.target")
        client.succeed("echo ping | socat -t5 - UDP:appliance:51820 | grep -qx ping")
        scan = client.succeed(
            "nmap -Pn -sT --max-retries 1 -p 1-1024,2375,2376,5000,8080,8081 appliance"
        )
        assert not re.search(r"^\d+/tcp\s+open", scan, re.M), scan

    with subtest("autoUpgrade: daily at the reboot time, allowReboot, the seed's window"):
        appliance.succeed("systemctl cat nixos-upgrade.timer | grep -qx 'OnCalendar=03:30'")
        script = appliance.succeed(
            "cat $(systemctl show -P ExecStart nixos-upgrade.service | grep -o '/nix/store/[^ ;]*' | head -1)"
        )
        for want in [
            'lower="03:30"',
            'upper="04:30"',
            "/bin/shutdown -r +1",
            "/run/booted-system/{initrd,kernel,kernel-modules}",
            "--update-input appliance --update-input appliance/nixpkgs",
            "--flake /etc/homelab-ops/appliance#appliance",
            "exec 9>/run/homelab-appliance-rebuild.lock",
        ]:
            assert want in script, f"nixos-upgrade's script lacks {want!r}:\n{script}"
        # The lock comes before the rebuild.
        assert script.index("flock 9") < script.index("nixos-rebuild"), script

    with subtest("a settings change defers to a boot default that isn't the running system"):
        settings_json = "/etc/homelab-ops/appliance/settings.json"
        profile = "/nix/var/nix/profiles/system"
        appliance.succeed(f"cp {settings_json} /tmp/settings.orig")
        # A test VM may boot without a system profile; "" then.
        old_link = appliance.succeed(f"readlink {profile} || true").strip()
        # Stand in for nixos-upgrade's `boot` outside the reboot window: the
        # boot default moves to another store path than the running system.
        appliance.succeed(
            f"ln -sfn $(readlink -f /run/current-system/sw) {profile}-99-link"
            f" && ln -sfn {profile}-99-link {profile}.tmp && mv -T {profile}.tmp {profile}"
        )
        # In place, so the path unit sees the write.
        appliance.succeed(
            f"jq -c '.rebootTime = \"05:00\"' {settings_json} >/tmp/settings.json"
            f" && cat /tmp/settings.json >{settings_json}"
        )
        appliance.wait_until_succeeds(
            "journalctl -u homelab-appliance-settings.service | grep -q 'deferring '"
        )
        appliance.fail("journalctl -u homelab-appliance-settings.service | grep -q 'applying '")
        appliance.succeed(f"test $(readlink {profile}) = {profile}-99-link")
        # Put both back.
        if old_link:
            appliance.succeed(f"ln -sfn {old_link} {profile}.tmp && mv -T {profile}.tmp {profile}")
        else:
            appliance.succeed(f"rm {profile}")
        appliance.succeed(f"rm {profile}-99-link")
        appliance.succeed(f"cat /tmp/settings.orig >{settings_json}")
        appliance.wait_until_succeeds(
            "test $(journalctl -u homelab-appliance-settings.service | grep -c 'is already applied') -ge 2"
        )
  '';
}
