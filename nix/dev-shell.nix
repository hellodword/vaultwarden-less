{
  pkgs,
  go,
  treefmt,
}:

pkgs.mkShell {
  packages = with pkgs; [
    go
    gcc
    restic
    docker
    docker-compose
    docker-buildx
    treefmt.config.build.wrapper
    golangci-lint
    actionlint
    shellcheck
    sqlite
    jq
    python3
    openssh
    sshpass
    bitwarden-cli
  ];

  GOTOOLCHAIN = "local";
  CGO_ENABLED = "1";
}
