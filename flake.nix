{
  description = "vaultwarden-less v1 reproducible development checks";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    treefmt-nix.url = "github:numtide/treefmt-nix";
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      go1267 = pkgs.stdenv.mkDerivation {
        pname = "go";
        version = "1.26.7";
        src = pkgs.fetchurl {
          url = "https://go.dev/dl/go1.26.7.linux-amd64.tar.gz";
          hash = "sha256-/7X43hDGJVDf3atms2tXAwch4KRKMhjp4Rgde1nxIco=";
        };
        nativeBuildInputs = [ pkgs.makeWrapper ];
        dontConfigure = true;
        dontBuild = true;
        installPhase = ''
          mkdir -p "$out/lib/go"
          cp -R . "$out/lib/go"
          makeWrapper "$out/lib/go/bin/go" "$out/bin/go" --set GOROOT "$out/lib/go"
          makeWrapper "$out/lib/go/bin/gofmt" "$out/bin/gofmt" --set GOROOT "$out/lib/go"
        '';
      };
      treefmt = treefmt-nix.lib.evalModule pkgs {
        projectRootFile = "flake.nix";
        programs.gofmt.enable = true;
        programs.nixfmt.enable = true;
      };
      tools = with pkgs; [
        go1267
        gcc
        just
        restic
        golangci-lint
        actionlint
        shellcheck
        docker
        docker-compose
        sqlite
        jq
        treefmt.config.build.wrapper
        nixfmt
      ];
    in
    {
      devShells.${system}.default = pkgs.mkShell {
        packages = tools;
        shellHook = ''
          export GOTOOLCHAIN=local
          export CGO_ENABLED=1
        '';
      };

      formatter.${system} = treefmt.config.build.wrapper;

      checks.${system} = {
        toolchain =
          pkgs.runCommand "vaultwarden-less-toolchain-check"
            {
              nativeBuildInputs = [ go1267 ];
              GOTOOLCHAIN = "local";
            }
            ''
              test "$(go version)" = "go version go1.26.7 linux/amd64"
              test "$(go env GOTOOLCHAIN)" = "local"
              touch "$out"
            '';

        format =
          pkgs.runCommand "vaultwarden-less-format-check"
            {
              src = ./.;
              nativeBuildInputs = [ treefmt.config.build.wrapper ];
            }
            ''
              cd "$src"
              treefmt --ci
              touch "$out"
            '';
      };
    };
}
