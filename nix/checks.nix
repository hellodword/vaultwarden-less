{
  formatter,
  inputs,
  packages,
  pkgs,
  source,
  upstreams,
}:

let
  lib = pkgs.lib;
  versionFromTag = prefix: tag: lib.removePrefix prefix tag;
  sourceRevMatches =
    package: tag:
    let
      revision = package.src.rev or "";
    in
    revision == tag || revision == "refs/tags/${tag}";
  goSourceURLs = pkgs.go.src.urls or [ (pkgs.go.src.url or "") ];
  goCheck =
    {
      name,
      command,
      extraInputs ? [ ],
    }:
    pkgs.runCommand name
      {
        nativeBuildInputs = [
          pkgs.go
          pkgs.gcc
        ]
        ++ extraInputs;
        GOTOOLCHAIN = "local";
        CGO_ENABLED = "1";
      }
      ''
        export GOCACHE="$TMPDIR/go-cache"
        export GOMODCACHE="$TMPDIR/go-modules"
        export XDG_CACHE_HOME="$TMPDIR/xdg-cache"
        export GOFLAGS="-mod=vendor"
        export GOPROXY="off"
        export GOSUMDB="off"
        worktree="$TMPDIR/source"
        mkdir -m 0700 "$GOCACHE" "$GOMODCACHE" "$XDG_CACHE_HOME" "$worktree"
        cp -R --no-preserve=mode ${source}/. "$worktree"
        test ! -e "$worktree/vendor"
        cp -R --no-preserve=mode ${packages.backup.goModules} "$worktree/vendor"
        chmod -R u+w "$worktree"
        cd "$worktree"
        ${command}
        touch "$out"
      '';
in
assert pkgs.vaultwarden.version == upstreams.vaultwarden.source_tag;
assert sourceRevMatches pkgs.vaultwarden upstreams.vaultwarden.source_tag;
assert pkgs.restic.version == versionFromTag "v" upstreams.restic.source_tag;
assert sourceRevMatches pkgs.restic upstreams.restic.source_tag;
assert pkgs.bitwarden-cli.version == versionFromTag "cli-v" upstreams.bitwarden_cli.source_tag;
assert sourceRevMatches pkgs.bitwarden-cli upstreams.bitwarden_cli.source_tag;
assert sourceRevMatches pkgs.bitwarden-cli.npmDeps upstreams.bitwarden_cli.source_tag;
assert pkgs.python3Packages.moto.version == upstreams.moto.source_tag;
assert sourceRevMatches pkgs.python3Packages.moto upstreams.moto.source_tag;
assert lib.all
  (
    name:
    pkgs.python3Packages.${name}.version == upstreams.moto.${name}.source_tag
    && sourceRevMatches pkgs.python3Packages.${name} upstreams.moto.${name}.source_tag
  )
  [
    "boto3"
    "botocore"
    "s3transfer"
  ];
assert pkgs.go.version == versionFromTag "go" upstreams.go.source_tag;
assert lib.any (lib.hasInfix "${upstreams.go.source_tag}.src.tar.gz") goSourceURLs;
{
  format = formatter.config.build.check inputs.self;

  unit = goCheck {
    name = "vaultwarden-less-unit";
    command = "go test ./... -count=1";
    extraInputs = [
      pkgs.docker
      pkgs.docker-compose
    ];
  };

  vet = goCheck {
    name = "vaultwarden-less-vet";
    command = "go vet ./...";
  };

  race = goCheck {
    name = "vaultwarden-less-race";
    command = "go test -race ./internal/worker ./internal/proxy -count=1";
  };

  script-tests =
    pkgs.runCommand "vaultwarden-less-script-tests"
      {
        nativeBuildInputs = [ pkgs.python3 ];
      }
      ''
        cd ${source}
        python3 tests/scripts/test_capture_vaultwarden_schema.py
        python3 tests/scripts/test_upstream_diff.py
        touch "$out"
      '';
}
