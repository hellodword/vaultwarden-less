{
  description = "Auditable, locally built Vaultwarden backup and restore";

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
      pkgs = import nixpkgs { inherit system; };
      upstreams = builtins.fromTOML (builtins.readFile ./UPSTREAMS.toml);
      clean = self ? rev;
      projectCommit = if clean then self.rev else "";
      compactDate = self.lastModifiedDate;
      releaseDate =
        "${builtins.substring 0 4 compactDate}-${builtins.substring 4 2 compactDate}-${
          builtins.substring 6 2 compactDate
        }"
        + "T${builtins.substring 8 2 compactDate}-${builtins.substring 10 2 compactDate}-${
          builtins.substring 12 2 compactDate
        }Z";
      created =
        if clean then
          "${builtins.substring 0 4 compactDate}-${builtins.substring 4 2 compactDate}-${
            builtins.substring 6 2 compactDate
          }"
          + "T${builtins.substring 8 2 compactDate}:${builtins.substring 10 2 compactDate}:${
            builtins.substring 12 2 compactDate
          }Z"
        else
          "1970-01-01T00:00:01Z";
      release =
        if clean then "RELEASE.${releaseDate}-g${builtins.substring 0 12 projectCommit}" else "development";
      identity = {
        inherit
          clean
          created
          projectCommit
          release
          ;
      };
      formatter = import ./nix/formatter.nix {
        inherit pkgs treefmt-nix;
      };
      packages = import ./nix/packages.nix {
        inherit
          identity
          pkgs
          upstreams
          ;
        source = ./.;
      };
    in
    {
      packages.${system} = packages // {
        default = packages.backup;
      };

      devShells.${system}.default = import ./nix/dev-shells.nix {
        inherit formatter pkgs;
      };

      formatter.${system} = formatter.config.build.wrapper;

      checks.${system} = import ./nix/checks.nix {
        inherit
          formatter
          inputs
          packages
          pkgs
          upstreams
          ;
        source = ./.;
      };
    };
}
