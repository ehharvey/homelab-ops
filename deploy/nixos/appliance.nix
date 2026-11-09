# The web app appliance (#243, docs/Decisions.md §29, docs/Deployment.md §
# The appliance): NixOS running ../compose.yaml under Docker, set up at first
# boot from a NoCloud seed that `bootstrap render-appliance-seed` renders.
#
# Nothing operator-specific is in this module or the image built from it. The
# seed writes, through cloud-init's write_files:
#
#   /etc/homelab-ops/compose.yaml            ../compose.yaml, the image pinned by --image
#   /etc/homelab-ops/web/                    the deployment config directory
#   /etc/homelab-ops/appliance/settings.json the host settings below
#
# How the host settings take effect without rebuilding the image:
# /etc/homelab-ops/appliance/ is this box's own local flake. Its flake.nix
# (written at every activation, below) builds this flake's mkAppliance with
# settings.json next to it. homelab-appliance-settings.service compares
# settings.json with the settings the running system was built from, and on a
# difference runs `nixos-rebuild switch` on the local flake, against the
# running system's own sources (--override-input), so applying settings never
# moves the box to another version. The first boot does this once the seed is
# written; it needs the network, to substitute the few derivations the
# settings change from cache.nixos.org.
#
# Secrets: nothing secret ever enters the Nix store, which is world-readable.
# secrets/wireguard.key (docs/Decisions.md §28) arrives in the seed, or by
# hand with --no-secrets, straight into /etc/homelab-ops/web/secrets/.
# settings.json holds only the reboot time and SSH *public* keys, and is the
# only seed file the local flake reads. cloud-init's own cache of user-data,
# which would be a second copy of the key on this disk, is shredded once the
# first boot has written the seed's files (homelab-appliance-seed-scrub).
{
  config,
  lib,
  pkgs,
  modulesPath,
  ...
}:
let
  cfg = config.homelab.appliance;
  etcDir = "/etc/homelab-ops";
  configDir = "${etcDir}/web";
  composeFile = "${etcDir}/compose.yaml";
  flakeDir = "${etcDir}/appliance";
  settingsFile = "${flakeDir}/settings.json";
  appliedFile = "${etcDir}/settings.applied.json";
  # Written once cloud-init has consumed the seed and its cache is scrubbed;
  # cloud-init never runs again after it.
  seedConsumed = "/var/lib/homelab-ops/seed-consumed";
  wireguardPort = 51820;
  sshEnabled = cfg.settings.sshAuthorizedKeys != [ ];

  # HH:MM plus one hour, wrapping at midnight: the end of the reboot window.
  plusOneHour =
    t:
    let
      h = lib.toIntBase10 (builtins.substring 0 2 t);
      m = builtins.substring 3 2 t;
      h' = lib.mod (h + 1) 24;
    in
    "${lib.fixedWidthNumber 2 h'}:${m}";

  # The box's local flake: this flake's mkAppliance, with settings.json.
  localFlake = pkgs.writeText "flake.nix" ''
    # This appliance's NixOS configuration (#243). Generated: every activation
    # rewrites this file. The box's settings are settings.json beside it,
    # which the seed writes; flake.lock records the versions
    # nixos-upgrade.service last moved this box to.
    {
      inputs.appliance.url = "${cfg.upgradeFlake}";
      outputs =
        { appliance, ... }:
        {
          nixosConfigurations.appliance = appliance.lib.mkAppliance {
            settings =
              if builtins.pathExists ./settings.json then
                builtins.fromJSON (builtins.readFile ./settings.json)
              else
                { };
          };
        };
    }
  '';

  nixos-rebuild = "${config.system.build.nixos-rebuild}/bin/nixos-rebuild";
  compose = "${pkgs.docker-compose}/bin/docker-compose";

  # Two units run nixos-rebuild: nixos-upgrade and homelab-appliance-settings.
  # Each takes this lock first, for its whole run, so they take turns rather
  # than race on the system profile; one that finds the other running waits.
  rebuildLock = "/run/homelab-appliance-rebuild.lock";
  takeRebuildLock = ''
    exec 9>${rebuildLock}
    ${pkgs.util-linux}/bin/flock 9
  '';
in
{
  imports = [ "${modulesPath}/profiles/all-hardware.nix" ];

  options.homelab.appliance = {
    settings = {
      rebootTime = lib.mkOption {
        type = lib.types.strMatching "([01][0-9]|2[0-3]):[0-5][0-9]";
        default = "04:00";
        description = ''
          When nixos-upgrade runs each day, in the host's local time (UTC
          unless the operator sets a time zone), and the start of the
          one-hour window in which it may reboot. The seed's --reboot-time.
        '';
      };
      sshAuthorizedKeys = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = ''
          Public keys allowed to log in as root over SSH. Empty, the default,
          means no sshd and port 22 closed. The seed's --ssh-authorized-keys.
        '';
      };
    };
    upgradeFlake = lib.mkOption {
      type = lib.types.str;
      default = "github:ehharvey/homelab-ops?dir=deploy/nixos";
      description = "The flake nixos-upgrade follows: this repo's main.";
    };
    sources = {
      appliance = lib.mkOption {
        type = lib.types.str;
        internal = true;
        description = "Flake ref of the appliance flake this system was built from.";
      };
      nixpkgs = lib.mkOption {
        type = lib.types.str;
        internal = true;
        description = "Flake ref of the nixpkgs this system was built from.";
      };
    };
  };

  config = {
    system.stateVersion = "26.05";
    networking.hostName = lib.mkDefault "homelab-appliance";

    # The disk layout make-disk-image's "efi" table writes (./image.nix), so
    # the image's system and a rebuild on the box agree.
    fileSystems."/" = {
      device = "/dev/disk/by-label/nixos";
      fsType = "ext4";
      autoResize = true;
    };
    fileSystems."/boot" = {
      device = "/dev/disk/by-label/ESP";
      fsType = "vfat";
      options = [
        "fmask=0077"
        "dmask=0077"
      ];
    };
    boot.growPartition = true;
    # Each generation stays bootable from the systemd-boot menu: the rollback
    # path for a bad upgrade. The image's ESP is 1 GiB (./image.nix).
    boot.loader.systemd-boot.enable = true;
    boot.loader.systemd-boot.configurationLimit = 10;
    # A flashed disk: boot from the removable-media path, and write no EFI
    # variables.
    boot.loader.efi.canTouchEfiVariables = false;
    hardware.enableRedistributableFirmware = true;
    # So `incus exec` works when the image runs as an Incus VM, as the validate
    # script runs it. It does nothing on a laptop: the agent only starts from
    # Incus's agent drive.
    virtualisation.incus.agent.enable = true;
    # Without that drive's serial port (a laptop, the nixosTest's QEMU), skip
    # the agent rather than restart it forever. The port's name is Incus's
    # osVioSerialPath for Linux.
    systemd.services.incus-agent.unitConfig.ConditionPathExists = "/dev/virtio-ports/org.linuxcontainers.incus";

    # A laptop on the nodes' LAN (docs/Decisions.md §31): wired, never asleep.
    networking.useDHCP = lib.mkDefault true;
    services.logind.settings.Login = {
      HandleLidSwitch = "ignore";
      HandleLidSwitchExternalPower = "ignore";
      HandleLidSwitchDocked = "ignore";
      HandleSuspendKey = "ignore";
      HandleHibernateKey = "ignore";
      IdleAction = "ignore";
    };
    systemd.targets = lib.genAttrs [ "sleep" "suspend" "hibernate" "hybrid-sleep" ] (_: {
      enable = false;
    });

    # The firewall opens the WireGuard UDP port, plus SSH only when keys are
    # set. Docker-published ports bypass this INPUT firewall (they are DNATed
    # to the container), so what really limits the box to WireGuard UDP is
    # compose.yaml publishing nothing else. Opening it here says the same and
    # holds if the web app ever runs on the host network.
    networking.firewall = {
      enable = true;
      allowedUDPPorts = [ wireguardPort ];
      allowedTCPPorts = lib.optional sshEnabled 22;
    };
    services.openssh = {
      enable = sshEnabled;
      openFirewall = false;
      settings = {
        PasswordAuthentication = false;
        KbdInteractiveAuthentication = false;
        PermitRootLogin = "prohibit-password";
      };
    };
    users.users.root.openssh.authorizedKeys.keys = cfg.settings.sshAuthorizedKeys;

    # Docker restarts the web container at boot (restart: unless-stopped).
    # On a re-seed's boot it must start from the new seed's files, not the
    # old: hold Docker until cloud-init's bootcmd and write_files have run.
    # After the seed is consumed cloud-init.service is skipped, so this
    # costs later boots nothing.
    systemd.services.docker.after = [ "cloud-init.service" ];

    # The seed: a FAT or ISO volume labelled CIDATA, read once, at the first
    # boot. manual_cache_clean keeps cloud-init from looking for a new seed
    # should it run again; it doesn't, since homelab-appliance-seed-scrub
    # turns it off once the seed is consumed. So the stick, which holds the
    # secret, can come out. cloud-init leaves the network, the disk and SSH
    # host keys to NixOS.
    services.cloud-init = {
      enable = true;
      network.enable = false;
      settings = {
        datasource_list = [ "NoCloud" ];
        manual_cache_clean = true;
        preserve_hostname = true;
        growpart.mode = "off";
        resize_rootfs = false;
        ssh_deletekeys = false;
        ssh_genkeytypes = [ ];
        # NixOS's default final modules, less the ones that only warn here
        # (keys-to-console's helper isn't packaged), so a good first boot
        # reports `status: done` with no recoverable errors.
        cloud_final_modules = [
          "scripts-vendor"
          "scripts-per-once"
          "scripts-per-boot"
          "scripts-per-instance"
          "scripts-user"
          "final-message"
          "power-state-change"
        ];
      };
    };

    # cloud-init's cache holds user-data whole, secrets/wireguard.key
    # included, for the box's lifetime: user-data.txt, its processed .i,
    # cloud-config.txt, and the pickled datasource obj.pkl. Once the first
    # boot's final stage is done, shred them and mark the seed consumed, so
    # cloud-init never runs again (it would want that cache). Its /run copy
    # (instance-data-sensitive.json, root-only) goes with the first reboot.
    # To re-seed a box (over SSH): remove ${seedConsumed}, run
    # `cloud-init clean`, and reboot with the new stick. The seed's bootcmd
    # empties ${configDir} first, so the new seed's directory replaces the
    # old one whole.
    systemd.services.cloud-init-local.unitConfig.ConditionPathExists = "!${seedConsumed}";
    systemd.services.cloud-init.unitConfig.ConditionPathExists = "!${seedConsumed}";
    systemd.services.cloud-config.unitConfig.ConditionPathExists = "!${seedConsumed}";
    systemd.services.cloud-final.unitConfig.ConditionPathExists = "!${seedConsumed}";
    systemd.services.homelab-appliance-seed-scrub = {
      description = "Shred cloud-init's cached copy of the seed's user-data";
      wantedBy = [ "multi-user.target" ];
      after = [ "cloud-final.service" ];
      unitConfig.ConditionPathExists = [
        "/var/lib/cloud/instance/boot-finished"
        "!${seedConsumed}"
      ];
      path = [ pkgs.coreutils ];
      serviceConfig.Type = "oneshot";
      script = ''
        for f in /var/lib/cloud/instances/*/{user-data.txt,user-data.txt.i,cloud-config.txt,obj.pkl}; do
          if [ -e "$f" ]; then
            shred --remove -- "$f"
            echo "shredded $f"
          fi
        done
        install -d -m 0755 "$(dirname ${seedConsumed})"
        touch ${seedConsumed}
      '';
    };

    virtualisation.docker = {
      enable = true;
      logDriver = "journald";
    };
    # For the operator over SSH, with a seed that carries SSH keys (without
    # them there is no shell: root has no password): `docker compose` for a
    # web image upgrade (docs/Deployment.md), jq for settings.json, and
    # `cloud-init status` for the first boot.
    environment.systemPackages = [
      pkgs.cloud-init
      pkgs.docker-compose
      pkgs.jq
    ];

    # The web app: compose.yaml from the seed, started once secrets/ exists.
    # With --no-secrets the seed leaves secrets/ out, and this waits until the
    # operator copies it in. Restarts after a reboot or a Docker restart are
    # Docker's, from compose.yaml's restart: unless-stopped.
    systemd.paths.homelab-ops-web = {
      description = "Wait for the web app's secrets/ before starting it";
      wantedBy = [ "multi-user.target" ];
      pathConfig.PathExists = "${configDir}/secrets";
    };
    systemd.services.homelab-ops-web = {
      description = "The homelab-ops web app (${composeFile})";
      wants = [
        "docker.service"
        "network-online.target"
      ];
      after = [
        "docker.service"
        "network-online.target"
        "cloud-init.service"
      ];
      unitConfig.ConditionPathExists = [
        composeFile
        "${configDir}/secrets"
      ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        WorkingDirectory = etcDir;
        ExecStart = "${compose} --file ${composeFile} up --detach --remove-orphans";
        # The first start pulls the image, so it waits for the network.
        Restart = "on-failure";
        RestartSec = 30;
        TimeoutStartSec = "15min";
      };
    };

    # The box's local flake, and the settings it applies.
    nix.settings.experimental-features = [
      "nix-command"
      "flakes"
    ];
    nix.gc = {
      automatic = true;
      dates = "weekly";
      options = "--delete-older-than 30d";
    };
    system.activationScripts.homelab-appliance-flake = ''
      install -d -m 0755 ${flakeDir}
      install -m 0644 ${localFlake} ${flakeDir}/flake.nix
    '';
    # The settings this system was built with, defaults included, as
    # settings.json would spell them: render-appliance-seed always writes
    # both keys, so a seed at the defaults matches the generic image and
    # rebuilds nothing (checks.settings-defaults).
    environment.etc."homelab-ops/settings.applied.json".text = builtins.toJSON cfg.settings;
    systemd.paths.homelab-appliance-settings = {
      description = "Apply ${settingsFile} when it changes";
      wantedBy = [ "multi-user.target" ];
      pathConfig.PathChanged = settingsFile;
    };
    systemd.services.homelab-appliance-settings = {
      description = "Apply the seed's host settings (${settingsFile})";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [
        "network-online.target"
        "cloud-init.service"
      ];
      # nixos-rebuild switch runs from inside this unit: switching must not
      # stop or restart it.
      restartIfChanged = false;
      stopIfChanged = false;
      unitConfig.X-StopOnRemoval = false;
      environment = config.nix.envVars // {
        HOME = "/root";
      };
      path = [
        pkgs.coreutils
        pkgs.jq
        pkgs.gitMinimal
        config.nix.package.out
      ];
      # A failed rebuild (late DHCP, a cache.nixos.org hiccup) is retried
      # until it succeeds, rather than waiting for the next reboot or edit.
      # At the first boot multi-user.target waits for it meanwhile; the
      # console's getty and the web app don't.
      unitConfig.StartLimitIntervalSec = 0;
      serviceConfig = {
        Type = "oneshot";
        Restart = "on-failure";
        RestartSec = 60;
      };
      script = ''
        ${takeRebuildLock}
        if [ ! -e ${settingsFile} ]; then
          echo "no ${settingsFile}: keeping the image's defaults"
          exit 0
        fi
        if [ "$(jq -cS . ${settingsFile})" = "$(jq -cS . ${appliedFile})" ]; then
          echo "${settingsFile} is already applied"
          exit 0
        fi
        # A newer generation than the running one is the boot default: an
        # upgrade built outside the reboot window, waiting for its reboot.
        # (Or an older generation was picked from the boot menu.) Switching
        # to the running system's sources now would make an older system the
        # boot default, undoing that upgrade. Leave it be: that generation
        # applies the settings at its boot, and the next upgrade builds from
        # settings.json in any case.
        profile=$(readlink -f /nix/var/nix/profiles/system)
        running=$(readlink -f /run/current-system)
        if [ -e "$profile" ] && [ "$profile" != "$running" ]; then
          echo "deferring ${settingsFile}: the boot default ($profile) isn't the running system ($running); the settings apply at the next boot into it, or with the next upgrade"
          exit 0
        fi
        echo "applying ${settingsFile}"
        ${nixos-rebuild} switch --flake ${flakeDir}#appliance \
          --override-input appliance '${cfg.sources.appliance}' \
          --override-input appliance/nixpkgs '${cfg.sources.nixpkgs}' \
          --no-write-lock-file
        # At the first boot this runs before multi-user.target is reached,
        # and switch-to-configuration starts no unit the new system's
        # multi-user.target wants (sshd, when the seed carries keys), because
        # that target isn't active yet. Pull them in once this unit is done;
        # --no-block, since the target is ordered after this unit.
        ${config.systemd.package}/bin/systemctl start --no-block multi-user.target
      '';
    };

    # OS updates (docs/Decisions.md §29): daily, at the reboot time, from this
    # repo's main, with nixpkgs at the head of the release branch main's
    # flake.nix names. `--update-input appliance/nixpkgs` re-resolves that
    # nested input to its branch head, rather than to the commit main's
    # flake.lock pins, so a release's security patches arrive without a repo
    # PR; crossing to a new release takes a PR that changes the branch (#570).
    # The command it runs:
    #
    #   nixos-rebuild boot --refresh --flake /etc/homelab-ops/appliance#appliance \
    #     --update-input appliance --update-input appliance/nixpkgs
    #
    # It reboots only when the kernel, initrd or kernel modules changed, and
    # only inside the reboot window; otherwise it switches in place.
    systemd.services.nixos-upgrade.script = lib.mkBefore takeRebuildLock;
    system.autoUpgrade = {
      enable = true;
      flake = "${flakeDir}#appliance";
      flags = [
        "--update-input"
        "appliance"
        "--update-input"
        "appliance/nixpkgs"
      ];
      upgrade = false;
      dates = cfg.settings.rebootTime;
      allowReboot = true;
      rebootWindow = {
        lower = cfg.settings.rebootTime;
        upper = plusOneHour cfg.settings.rebootTime;
      };
    };
  };
}
