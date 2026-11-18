{ pkgs }:
pkgs
// {
  go = pkgs.go_1_27;
  buildGoModule = pkgs.buildGo127Module;
  bitwarden-cli = pkgs.bitwarden-cli.overrideAttrs (
    final: previous: {
      version = "2026.8.0";
      src = pkgs.fetchFromGitHub {
        owner = "bitwarden";
        repo = "clients";
        tag = "cli-v${final.version}";
        hash = "sha256-Kz7vy3mt4uLGA6wGtH527P5vY55UYSZtq6e8IyNJvc4=";
      };
      npmDepsHash = "sha256-5i6/TlqBhPLv00tN0sxFA/iRQ8QRyUxhCqYkhVBLz3w=";
      npmDeps = pkgs.fetchNpmDeps {
        name = "bitwarden-cli-${final.version}-npm-deps";
        inherit (final) src;
        inherit (previous) postPatch;
        hash = final.npmDepsHash;
        fetcherVersion = 2;
      };
    }
  );
}
