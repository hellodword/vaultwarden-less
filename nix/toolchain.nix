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
  inherit go treefmt;

  justApp = {
    type = "app";
    program = "${justWrapper}/bin/vaultwarden-less-just";
    meta.description = "Run the project Justfile with the pinned Just binary";
  };

  devPackages = with pkgs; [
    go
    gcc
    docker
    docker-compose
    treefmt.config.build.wrapper
    golangci-lint
    govulncheck
    trivy
    syft
    ruff
    python3
    bitwarden-cli
    git
    zstd
  ];

  deployPackages = with pkgs; [
    docker
    docker-compose
    python3
  ];
}
