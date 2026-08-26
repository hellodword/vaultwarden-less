{ pkgs }:

let
  wrapper = pkgs.writeShellApplication {
    name = "vaultwarden-less-just";
    runtimeInputs = [ pkgs.just ];
    text = ''
      exec just "$@"
    '';
  };
in
{
  type = "app";
  program = "${wrapper}/bin/vaultwarden-less-just";
  meta.description = "Run the project Justfile with the pinned Just binary";
}
