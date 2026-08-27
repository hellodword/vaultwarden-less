{
  pkgs,
  source,
}:

let
  justWrapper = pkgs.writeShellApplication {
    name = "vaultwarden-less-just";
    runtimeInputs = [
      pkgs.just
      pkgs.nix
    ];
    text = ''
      exec just "$@"
    '';
  };

  releaseWrapper = pkgs.writeShellApplication {
    name = "vaultwarden-less-release";
    runtimeInputs = [
      pkgs.docker
      pkgs.git
      pkgs.nix
      pkgs.python3
    ];
    text = ''
      exec python3 ${source}/scripts/release.py create "$@"
    '';
  };

  imageLoadWrapper = pkgs.writeShellApplication {
    name = "vaultwarden-less-image-load";
    runtimeInputs = [
      pkgs.docker
      pkgs.python3
    ];
    text = ''
      exec python3 ${source}/scripts/release.py load "$@"
    '';
  };
in
{
  just = {
    type = "app";
    program = "${justWrapper}/bin/vaultwarden-less-just";
    meta.description = "Run the project Justfile with pinned tools";
  };

  release = {
    type = "app";
    program = "${releaseWrapper}/bin/vaultwarden-less-release";
    meta.description = "Create a deterministic clean-commit image release";
  };

  image-load = {
    type = "app";
    program = "${imageLoadWrapper}/bin/vaultwarden-less-image-load";
    meta.description = "Verify and load a two-file image release";
  };
}
