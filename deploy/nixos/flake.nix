# The web app appliance's NixOS flake (#243, docs/Decisions.md §29,
# docs/Deployment.md § The appliance).
#
#   nixosModules.appliance        ./appliance.nix
#   lib.mkAppliance { settings }  the appliance's system, with a box's settings.json
#   nixosConfigurations.appliance mkAppliance with the defaults: the image's system
#   packages.x86_64-linux.raw-efi the generic disk image, the same for every operator
#   checks.x86_64-linux.appliance the nixosTest (./test.nix)
#   checks.x86_64-linux.settings-defaults the image's settings are a default seed's
#
# Build and check it with `make appliance-image` and `make appliance-check`,
# which run Nix in a pinned nixos/nix container. They pass the flake as
# path:<repo>/deploy?dir=nixos, so ../compose.yaml is inside its source tree.
#
# nixpkgs follows a stable release branch. Appliances track that branch's head
# themselves (appliance.nix's autoUpgrade); flake.lock pins what the image
# and the test are built from. Crossing to a new release is a PR changing the
# branch below (#570).
{
  description = "The homelab-ops web app appliance: NixOS running deploy/compose.yaml";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      inherit (nixpkgs) lib;
      pkgs = nixpkgs.legacyPackages.${system};

      # This flake's directory inside its source tree: "nixos" when built
      # from path:.../deploy?dir=nixos, "deploy/nixos" from GitHub.
      root = "${self.sourceInfo.outPath}";
      dir = lib.removePrefix "${root}/" (toString ./.);

      # The appliance's modules, with a box's settings.json as settings.
      applianceModules = settings: [
        self.nixosModules.appliance
        {
          homelab.appliance = {
            inherit settings;
            sources = {
              appliance = "path:${root}?dir=${dir}";
              nixpkgs = "path:${nixpkgs.outPath}";
            };
          };
        }
      ];

      mkAppliance =
        {
          settings ? { },
        }:
        lib.nixosSystem {
          inherit system;
          modules = applianceModules settings;
        };
    in
    {
      nixosModules.appliance = ./appliance.nix;

      lib = { inherit mkAppliance; };

      nixosConfigurations.appliance = mkAppliance { };

      packages.${system} = rec {
        raw-efi = import ./image.nix {
          inherit pkgs lib;
          inherit (self.nixosConfigurations.appliance) config;
        };
        default = raw-efi;
      };

      checks.${system} = {
        appliance = import ./test.nix {
          inherit pkgs lib applianceModules;
          composeFile = ../compose.yaml;
        };
        # The generic image's settings are what render-appliance-seed writes
        # with no flags (default-settings.json, which its Go test holds it
        # to), so a default seed rebuilds nothing at first boot.
        settings-defaults =
          let
            want = builtins.fromJSON (builtins.readFile ./default-settings.json);
            got = self.nixosConfigurations.appliance.config.homelab.appliance.settings;
          in
          assert lib.assertMsg (want == got)
            "the image's settings ${builtins.toJSON got} aren't default-settings.json's ${builtins.toJSON want}";
          pkgs.runCommand "settings-defaults" { } "touch $out";
      };
    };
}
