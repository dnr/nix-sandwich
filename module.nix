{ config, lib, pkgs, ... }:
let
  pkg = (import ./. { inherit pkgs; }).nix-sandwich-local;
in {
  # TODO: make a proper module with options instead of always-on

  nix.settings.trusted-substituters = [ "http://localhost:7419" ];

  # TODO: do it with socket activation
  systemd.services.nix-sandwich = {
    description = "nix download helper";
    wantedBy = [ "multi-user.target" ];
    serviceConfig.ExecStart = "${pkg}/bin/nix-sandwich";
    serviceConfig.DynamicUser = true;
    serviceConfig.LogsDirectory = "nix-sandwich-analytics";
    serviceConfig.TemporaryFileSystem = "/tmpfs:size=16G,mode=1777"; # force tmpfs
    environment.nix_sandwich_substituter_bind = "localhost:7419";
    environment.nix_sandwich_differ = lib.mkDefault (
      throw "must override systemd.services.nix-sandwich.environment.nix_sandwich_differ!");
    environment.TMPDIR = "/tmpfs";
  };
}
