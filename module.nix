{ config, lib, pkgs, ... }:
let
  defpkg = (import ./. { inherit pkgs; }).nix-sandwich-local;
  cfg = config.programs.nix-sandwich;
in with lib; {
  options = {
    programs.nix-sandwich = {
      enable = mkEnableOption "nix download helper";
      differ = mkOption {
        description = "url to remote differ";
        type = types.str;
        default = throw "must set config.programs.nix-sandwich.differ!";
        example = "https://abc213";
      };
      cache = mkOption {
        description = "url to remote diff cache";
        type = types.str;
        default = throw "must set config.programs.nix-sandwich.cache!";
        example = "https://abc213";
      };
      port = mkOption {
        description = "port to listen on";
        type = types.int;
        default = 7419;
      };
      package = mkOption {
        description = "nix-sandwich package";
        type = types.package;
        default = defpkg;
      };
    };
  };

  config = mkIf cfg.enable {
    nix.settings = {
      trusted-substituters = [ "http://localhost:${toString cfg.port}" ];
      # TODO: These are needed since we don't cache "recents" across restarts.
      # After we can do that, remove this.
      narinfo-cache-positive-ttl = 15;
      narinfo-cache-negative-ttl = 15;
    };

    systemd.sockets.nix-sandwich = {
      description = "nix download helper activation socket";
      wantedBy = [ "sockets.target" ];
      socketConfig.ListenStream = "127.0.0.1:${toString cfg.port}";
    };

    systemd.services.nix-sandwich = {
      description = "nix download helper";
      serviceConfig.ExecStart = "${cfg.package}/bin/nix-sandwich";
      serviceConfig.Type = "notify";
      serviceConfig.NotifyAccess = "all";
      serviceConfig.DynamicUser = true;
      serviceConfig.LogsDirectory = "nix-sandwich-analytics";
      serviceConfig.TemporaryFileSystem = "/tmpfs:size=16G,mode=1777"; # force tmpfs
      environment.nix_sandwich_subst_idle_time = "15m";
      environment.nix_sandwich_differ = cfg.differ;
      environment.nix_sandwich_cache_read_url = cfg.cache;
      environment.TMPDIR = "/tmpfs";
    };
  };
}
