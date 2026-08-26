{
  inputs,
  pkgs,
  go,
  python,
  treefmt,
  packages,
  source,
  upstreams,
}:

{
  format = treefmt.config.build.check inputs.self;

  unit = packages.backup;

  generated =
    pkgs.runCommand "vaultwarden-less-generated-upstreams"
      {
        nativeBuildInputs = [
          python
          go
        ];
      }
      ''
        python3 ${source}/scripts/generate_upstreams.py \
          --input ${source}/UPSTREAMS.toml \
          --output generated.go
        gofmt -w generated.go
        cmp generated.go ${source}/internal/upstream/generated.go
        touch "$out"
      '';

  python =
    pkgs.runCommand "vaultwarden-less-python-check"
      {
        nativeBuildInputs = [ pkgs.ruff ];
      }
      ''
        ruff check ${source}/scripts ${source}/tests/scripts
        ruff format --check ${source}/scripts ${source}/tests/scripts
        touch "$out"
      '';

  script-tests =
    pkgs.runCommand "vaultwarden-less-script-tests"
      {
        nativeBuildInputs = [ python ];
      }
      ''
        cd ${source}
        python3 -m unittest discover --start-directory tests/scripts --pattern 'test_*.py'
        touch "$out"
      '';

  toolchain =
    pkgs.runCommand "vaultwarden-less-toolchain-wiring"
      {
        nativeBuildInputs = [
          go
          packages."restic-patched"
          pkgs.docker
          pkgs.docker-compose
          pkgs.golangci-lint
          pkgs.govulncheck
          pkgs.trivy
          pkgs.syft
          python
          pkgs.ruff
          pkgs.bitwarden-cli
        ];
        GOTOOLCHAIN = "local";
        CGO_ENABLED = "1";
      }
      ''
        export HOME="$TMPDIR/home"
        export BITWARDENCLI_APPDATA_DIR="$TMPDIR/bitwarden-cli"
        mkdir -m 0700 "$HOME" "$BITWARDENCLI_APPDATA_DIR"
        test "$(go version)" = "go version go${upstreams.go.version} linux/amd64"
        test "$(go env GOTOOLCHAIN)" = "local"
        test "$(restic version | cut -d' ' -f2)" = "${upstreams.restic.version}"
        test "$(bw --version)" = "${upstreams.bitwarden_cli.version}"
        test -x "$(command -v docker)"
        test -x "$(command -v govulncheck)"
        test -x "$(command -v trivy)"
        test -x "$(command -v syft)"
        touch "$out"
      '';
}
