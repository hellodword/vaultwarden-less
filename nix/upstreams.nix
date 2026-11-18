{ pkgs }:
pkgs
// {
  go = pkgs.go_1_27;
  buildGoModule = pkgs.buildGo127Module;
}
