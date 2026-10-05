{
  description = "Vaultwarden request proxy and native SQLite/restic backup";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };
  outputs =
    inputs@{
      self,
      nixpkgs,
      treefmt-nix,
      ...
    }:
    let
      system = "x86_64-linux";
      pkgs = import ./nix/upstreams.nix { pkgs = import nixpkgs { inherit system; }; };
      identity = {
        projectCommit = self.rev or self.dirtyRev or "";
        release = if self ? rev then "git-${self.rev}" else "development";
        created = "1970-01-01T00:00:01Z";
      };
      formatter = import ./nix/formatter.nix { inherit pkgs treefmt-nix; };
      packages = import ./nix/packages.nix {
        inherit identity pkgs;
        source = ./.;
      };
    in
    {
      packages.${system} = packages // {
        default = packages.backup;
      };
      devShells.${system}.default = import ./nix/dev-shells.nix { inherit formatter pkgs; };
      formatter.${system} = formatter.config.build.wrapper;
      apps.${system} = import ./nix/apps.nix {
        inherit packages pkgs;
        source = ./.;
      };
      checks.${system} = import ./nix/checks.nix {
        inherit
          formatter
          inputs
          packages
          pkgs
          ;
        source = ./.;
      };
    };
}
