{ pkgs, upstreams }:

let
  python3 = pkgs.python3.override {
    packageOverrides =
      pythonFinal: pythonPrev:
      pkgs.lib.genAttrs [ "boto3" "botocore" "s3transfer" ] (
        name:
        pythonPrev.${name}.overridePythonAttrs {
          version = upstreams.moto.${name}.source_tag;
          src = pkgs.fetchFromGitHub {
            owner = "boto";
            repo = name;
            tag = upstreams.moto.${name}.source_tag;
            hash = upstreams.moto.${name}.source_hash;
          };
        }
      )
      // {
        moto = pythonPrev.moto.overridePythonAttrs (previousAttrs: {
          version = upstreams.moto.source_tag;
          src = pkgs.fetchFromGitHub {
            owner = "getmoto";
            repo = "moto";
            tag = upstreams.moto.source_tag;
            hash = upstreams.moto.source_hash;
          };
          # Botocore 1.43.93 preserves fractional Unix timestamps. Adapt the
          # fixture as in getmoto/moto#10259, retaining strict equality for
          # our pinned newer SDK instead of accepting both precision modes.
          postPatch = (previousAttrs.postPatch or "") + ''
            substituteInPlace tests/test_scheduler/__init__.py \
              --replace-fail 'to_append.replace(microsecond=0)' 'to_append'
          '';
        });
      };
  };
in
pkgs
// {
  # nixpkgs still defaults to the preceding Go release.
  # Scope the compiler change to this project's builds and checks.
  go = pkgs.go_1_27;
  buildGoModule = pkgs.buildGo127Module;

  # Pin reviewed compatible releases independently of nixpkgs defaults, retaining
  # its source builds, license exclusions, dependency handling, and package tests.
  bitwarden-cli = pkgs.bitwarden-cli.overrideAttrs (
    finalAttrs: previousAttrs: {
      version = pkgs.lib.removePrefix "cli-v" upstreams.bitwarden_cli.source_tag;
      src = pkgs.fetchFromGitHub {
        owner = "bitwarden";
        repo = "clients";
        tag = upstreams.bitwarden_cli.source_tag;
        hash = upstreams.bitwarden_cli.source_hash;
      };
      npmDepsHash = upstreams.bitwarden_cli.npm_deps_hash;
      npmDeps = pkgs.fetchNpmDeps {
        name = "bitwarden-cli-${finalAttrs.version}-npm-deps";
        inherit (finalAttrs) src;
        inherit (previousAttrs) postPatch;
        hash = finalAttrs.npmDepsHash;
        fetcherVersion = 2;
      };
    }
  );

  inherit python3;
  python3Packages = python3.pkgs;
}
