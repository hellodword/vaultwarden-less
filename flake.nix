{
  description = "vaultwarden-less development environment";

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
      go = import ./nix/go-1.26.7.nix { inherit pkgs; };
      treefmt = import ./nix/treefmt.nix {
        inherit pkgs treefmt-nix;
      };
    in
    {
      devShells.${system}.default = import ./nix/dev-shell.nix {
        inherit pkgs go treefmt;
      };

      formatter.${system} = treefmt.config.build.wrapper;

      checks.${system} = import ./nix/checks.nix {
        inherit
          inputs
          pkgs
          go
          treefmt
          ;
      };

      apps.${system}.just = import ./nix/just-app.nix { inherit pkgs; };
    };
}
