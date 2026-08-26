{
  pkgs,
  treefmt-nix,
}:

treefmt-nix.lib.evalModule pkgs {
  projectRootFile = "flake.nix";
  programs.gofmt.enable = true;
  programs.nixfmt.enable = true;
}
