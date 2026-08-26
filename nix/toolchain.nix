{
  pkgs,
  treefmt-nix,
  upstreams,
}:

let
  go = pkgs.stdenv.mkDerivation {
    pname = "go";
    version = upstreams.go.version;

    src = pkgs.fetchurl {
      url = upstreams.go.source_url;
      hash = upstreams.go.source_hash;
    };

    nativeBuildInputs = [ pkgs.makeWrapper ];
    dontConfigure = true;
    dontBuild = true;
    passthru = {
      inherit (pkgs.go_1_26) GOOS GOARCH CGO_ENABLED;
    };

    installPhase = ''
      runHook preInstall
      mkdir -p "$out/lib/go"
      cp -R . "$out/lib/go"
      makeWrapper "$out/lib/go/bin/go" "$out/bin/go" --set GOROOT "$out/lib/go"
      makeWrapper "$out/lib/go/bin/gofmt" "$out/bin/gofmt" --set GOROOT "$out/lib/go"
      runHook postInstall
    '';
  };

  python = pkgs.python3.withPackages (pythonPackages: [ pythonPackages.pyyaml ]);

  treefmt = treefmt-nix.lib.evalModule pkgs {
    projectRootFile = "flake.nix";
    programs.gofmt.enable = true;
    programs.nixfmt.enable = true;
    programs.ruff-check.enable = true;
    programs.ruff-format.enable = true;
  };

  justWrapper = pkgs.writeShellApplication {
    name = "vaultwarden-less-just";
    runtimeInputs = [ pkgs.just ];
    text = ''
      exec just "$@"
    '';
  };
in
{
  inherit go python treefmt;

  justApp = {
    type = "app";
    program = "${justWrapper}/bin/vaultwarden-less-just";
    meta.description = "Run the project Justfile with the pinned Just binary";
  };

  devPackages = [
    go
    pkgs.gcc
    pkgs.docker
    pkgs.docker-compose
    treefmt.config.build.wrapper
    pkgs.golangci-lint
    pkgs.govulncheck
    pkgs.trivy
    pkgs.syft
    pkgs.ruff
    python
    pkgs.bitwarden-cli
    pkgs.git
    pkgs.zstd
  ];

  deployPackages = [
    pkgs.docker
    pkgs.docker-compose
    python
  ];
}
