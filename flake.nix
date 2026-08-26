{
  description = "Auditable, locally built Vaultwarden backup and restore";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    treefmt-nix.url = "github:numtide/treefmt-nix";
  };

  outputs =
    inputs@{
      nixpkgs,
      treefmt-nix,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      upstreams = builtins.fromTOML (builtins.readFile ./UPSTREAMS.toml);
      productVersion = builtins.replaceStrings [ "\n" ] [ "" ] (builtins.readFile ./VERSION);
      toolchain = import ./nix/toolchain.nix {
        inherit pkgs treefmt-nix upstreams;
      };
      packages = import ./nix/packages.nix {
        inherit pkgs upstreams productVersion;
        inherit (toolchain) go;
        source = ./.;
      };
    in
    {
      packages.${system} = packages // {
        default = packages.backup;
      };

      devShells.${system} = {
        default = pkgs.mkShell {
          packages = toolchain.devPackages ++ [ packages."restic-patched" ];
          GOTOOLCHAIN = "local";
          CGO_ENABLED = "1";
        };

        deploy = pkgs.mkShell {
          packages = toolchain.deployPackages;
        };
      };

      formatter.${system} = toolchain.treefmt.config.build.wrapper;

      checks.${system} = import ./nix/checks.nix {
        inherit
          inputs
          pkgs
          packages
          upstreams
          ;
        inherit (toolchain) go treefmt;
        source = ./.;
      };

      apps.${system}.just = toolchain.justApp;
    };
}
