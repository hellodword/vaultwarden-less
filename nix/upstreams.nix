{ pkgs }:
let
  # These are build inputs for the test tools. Keep compatibility pins beside
  # the overrides that consume them; Vaultwarden and restic come from nixpkgs.
  sdk = {
    boto3 = {
      version = "1.43.107";
      hash = "sha256-d8Vr7Vh234J7rWTdPCW5g7wZKTqvcm2vPEgvCWwqwYM=";
    };
    botocore = {
      version = "1.43.107";
      hash = "sha256-l0cHn0rqov6ayazKqCpIoqMX2P4DV9r0XgwTWeitrT4=";
    };
    s3transfer = {
      version = "0.19.2";
      hash = "sha256-BOK8kfTdxM6CInouXrBsQf4/zgL3l/A+VElh1VJj4mA=";
    };
  };
  python3 = pkgs.python3.override {
    packageOverrides =
      pythonFinal: pythonPrev:
      pkgs.lib.mapAttrs (
        name: pin:
        pythonPrev.${name}.overridePythonAttrs {
          inherit (pin) version;
          src = pkgs.fetchFromGitHub {
            owner = "boto";
            repo = name;
            tag = pin.version;
            inherit (pin) hash;
          };
        }
      ) sdk
      // {
        moto = pythonPrev.moto.overridePythonAttrs (previous: {
          version = "5.2.3";
          src = pkgs.fetchFromGitHub {
            owner = "getmoto";
            repo = "moto";
            tag = "5.2.3";
            hash = "sha256-7wE44MYdRf9p3/ac2qOPpLaS0bkpoh84R+rdJp5oycU=";
          };
          # The matching SDK preserves fractional scheduler timestamps.
          postPatch = (previous.postPatch or "") + ''
            substituteInPlace tests/test_scheduler/__init__.py \
              --replace-fail 'to_append.replace(microsecond=0)' 'to_append'
          '';
        });
      };
  };
in
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
  inherit python3;
  python3Packages = python3.pkgs;
}
