{
  formatter,
  pkgs,
}:

pkgs.mkShell {
  packages = [
    pkgs.go
    pkgs.gcc
    pkgs.docker
    pkgs.docker-compose
    formatter.config.build.wrapper
    pkgs.ruff
    pkgs.python3
    pkgs.bitwarden-cli
    pkgs.git
    pkgs.gzip
    pkgs.restic
  ];
  GOTOOLCHAIN = "local";
  CGO_ENABLED = "1";
}
