{
  inputs,
  pkgs,
  go,
  treefmt,
}:

{
  format = treefmt.config.build.check inputs.self;

  toolchain =
    pkgs.runCommand "vaultwarden-less-toolchain-wiring"
      {
        nativeBuildInputs = with pkgs; [
          go
          gcc
          restic
          docker
          docker-compose
          docker-buildx
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
      ''
        test "$(go version)" = "go version go1.26.7 linux/amd64"
        test "$(go env GOTOOLCHAIN)" = "local"
        test "$(restic version | cut -d' ' -f2)" = "0.19.1"
        test -x "$(command -v docker)"
        test -x "$(command -v bw)"
        test -x "$(command -v python3)"
        touch "$out"
      '';
}
