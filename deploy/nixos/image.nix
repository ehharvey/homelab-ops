# The generic raw-efi disk image (#243): nixosConfigurations.appliance, the
# defaults alone, so one image serves every operator. Write it to the
# laptop's disk; the seed carries everything operator-specific. The root
# partition grows to fill the disk at first boot (boot.growPartition).
#
# make-disk-image directly, rather than system.build.images.raw-efi: that
# variant extends the configuration with its own disk module, so the image's
# system would differ from the one a rebuild on the box produces.
{
  pkgs,
  lib,
  config,
}:
import "${pkgs.path}/nixos/lib/make-disk-image.nix" {
  inherit pkgs lib config;
  name = "homelab-ops-appliance";
  baseName = "homelab-ops-appliance";
  format = "raw";
  partitionTableType = "efi";
  diskSize = "auto";
  additionalSpace = "1G";
  # Room for appliance.nix's configurationLimit of generations.
  bootSize = "1G";
  # A flake system: no channel.
  copyChannel = false;
}
