{ pkgs ? import <nixpkgs> { } }:
rec {
  src = {
    pname = "nix-sandwich";
    version = "0.0.4";
    vendorHash = "sha256-Hk1soElRrn+VwI+feDy8XzIVI2wVNM6LU8BNPsOivEs=";
    src = pkgs.lib.sourceByRegex ./. [ ".*.go" "go.(mod|sum)" ];
  };

  nix-sandwich-local = pkgs.buildGoModule (src // {
    buildInputs = with pkgs; [
      brotli.dev
    ];
    ldflags = with pkgs; [
      "-X main.catBin=${coreutils}/bin/cat"
      "-X main.gzipBin=${gzip}/bin/gzip"
      "-X main.nixBin=${nix}/bin/nix"
      "-X main.xzBin=${xz}/bin/xz"
      "-X main.zstdBin=${zstd}/bin/zstd"
    ];
  });

  nix-sandwich-image = pkgs.buildGoModule (src // {
    tags = [ "lambda.norpc" ];
    # CGO is only needed for cbrotli, which is only used on the client side.
    # Disabling CGO shrinks the binary a little more.
    CGO_ENABLED = "0";
    ldflags = [
      # "-s" "-w"  # only saves 3.6% of image size
      "-X main.gzipBin=${gzStaticBin}/bin/gzip"
      "-X main.xzBin=${xzStaticBin}/bin/xz"
      "-X main.zstdBin=${zstdStaticBin}/bin/zstd"
    ];
  });

  # Patch zstd to add --dict-stream-size.
  withStreamPatch = zstd: (zstd.overrideAttrs (old: {
    patches = old.patches ++ [ ./zstd-streaming-dict.patch ];
  })).override { buildContrib = false; doCheck = false; };
  zstdStream = withStreamPatch pkgs.zstd;
  zstdStreamStatic = withStreamPatch pkgs.pkgsStatic.zstd;

  # Use static binaries and take only the main binaries to make the image as
  # small as possible:
  zstdStaticBin = pkgs.stdenv.mkDerivation {
    name = "zstd-binonly";
    src = zstdStreamStatic;
    installPhase = "mkdir -p $out/bin && cp $src/bin/zstd $out/bin/";
  };
  xzStaticBin = pkgs.stdenv.mkDerivation {
    name = "xz-binonly";
    src = pkgs.pkgsStatic.xz;
    installPhase = "mkdir -p $out/bin && cp $src/bin/xz $out/bin/";
  };
  gzStaticBin = pkgs.stdenv.mkDerivation {
    name = "gzip-binonly";
    src = pkgs.pkgsStatic.gzip;
    installPhase = "mkdir -p $out/bin && cp $src/bin/.gzip-wrapped $out/bin/gzip";
  };

  image = pkgs.dockerTools.streamLayeredImage {
    name = "lambda";
    # TODO: can we make it run on arm?
    # architecture = "arm64";
    # Not needed for now. Maybe if we allow configurable upstreams.
    #contents = [
    #  pkgs.cacert
    #];
    config = {
      User = "1000:1000";
      Cmd = [ "${nix-sandwich-image}/bin/nix-sandwich" ];
    };
  };

  shell = pkgs.mkShell {
    buildInputs = with pkgs; [
      awscli2
      go
      gzip
      jq
      nix
      skopeo
      terraform
      xdelta
      xz
      zstdStream
      # for cbrotli:
      brotli.dev
      gcc
    ];
  };
}
