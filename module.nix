{ config, lib, pkgs, ... }:
let
  pkg = (import ./. { inherit pkgs; }).nix-sandwich-local;
in {
  # TODO: make a proper module with options instead of always-on

  nix.settings = {
    trusted-substituters = [ "http://localhost:7419" ];
    # TODO: These are needed since we don't cache "recents" across restarts.
    # After we can do that, remove this.
    narinfo-cache-positive-ttl = 15;
    narinfo-cache-negative-ttl = 15;
  };

  systemd.sockets.nix-sandwich = {
    description = "nix download helper activation socket";
    wantedBy = [ "sockets.target" ];
    socketConfig.ListenStream = "127.0.0.1:7419";
  };

  systemd.services.nix-sandwich = {
    description = "nix download helper";
    serviceConfig.ExecStart = "${pkg}/bin/nix-sandwich";
    serviceConfig.Type = "notify";
    serviceConfig.DynamicUser = true;
    serviceConfig.LogsDirectory = "nix-sandwich-analytics";
    serviceConfig.TemporaryFileSystem = "/tmpfs:size=16G,mode=1777"; # force tmpfs
    environment.nix_sandwich_subst_idle_time = "15m";
    environment.nix_sandwich_differ = lib.mkDefault (
      throw "must override systemd.services.nix-sandwich.environment.nix_sandwich_differ!");
    environment.TMPDIR = "/tmpfs";
  };
}
