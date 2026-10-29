{
  formatter,
  inputs,
  packages,
  pkgs,
  source,
}:
let
  goCheck =
    name: command:
    pkgs.runCommand name
      {
        nativeBuildInputs = [
          pkgs.go
          pkgs.gcc
          pkgs.sqlite
          pkgs.restic
        ];
        GOTOOLCHAIN = "local";
        CGO_ENABLED = "1";
      }
      ''
        export GOCACHE="$TMPDIR/go-cache"
        export GOMODCACHE="$TMPDIR/go-modules"
        export XDG_CACHE_HOME="$TMPDIR/xdg-cache"
        export GOFLAGS="-mod=vendor" GOPROXY="off" GOSUMDB="off"
        mkdir -m 0700 "$GOCACHE" "$GOMODCACHE" "$XDG_CACHE_HOME" "$TMPDIR/source"
        cp -R --no-preserve=mode ${source}/. "$TMPDIR/source"
        cp -R --no-preserve=mode ${packages.backup.goModules} "$TMPDIR/source/vendor"
        chmod -R u+w "$TMPDIR/source"
        cd "$TMPDIR/source"
        ${command}
        touch "$out"
      '';
in
{
  format = formatter.config.build.check inputs.self;
  unit = goCheck "vaultwarden-less-unit" "go test ./... -count=1";
  vet = goCheck "vaultwarden-less-vet" "go vet ./...";
  race = goCheck "vaultwarden-less-race" "go test -race ./internal/... ./cmd/... ./tests/e2e/... -count=1";
  script-tests =
    pkgs.runCommand "vaultwarden-less-script-tests"
      {
        nativeBuildInputs = [
          pkgs.python3
          pkgs.just
        ];
      }
      ''
        cd ${source}
        python3 -m unittest discover -s tests/scripts -p 'test_*.py'
        python3 scripts/routes.py ${pkgs.vaultwarden.src} --output "$TMPDIR/catalog.txt"
        cmp internal/routes/catalog.txt "$TMPDIR/catalog.txt"
        touch "$out"
      '';
}
