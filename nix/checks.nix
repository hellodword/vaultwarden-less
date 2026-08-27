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
in
assert pkgs.vaultwarden.version == upstreams.vaultwarden.source_tag;
assert sourceRevMatches pkgs.vaultwarden upstreams.vaultwarden.source_tag;
assert pkgs.restic.version == versionFromTag "v" upstreams.restic.source_tag;
assert sourceRevMatches pkgs.restic upstreams.restic.source_tag;
assert pkgs.bitwarden-cli.version == versionFromTag "cli-v" upstreams.bitwarden_cli.source_tag;
assert sourceRevMatches pkgs.bitwarden-cli upstreams.bitwarden_cli.source_tag;
assert pkgs.python3Packages.moto.version == upstreams.moto.source_tag;
assert sourceRevMatches pkgs.python3Packages.moto upstreams.moto.source_tag;
assert pkgs.go.version == versionFromTag "go" upstreams.go.source_tag;
assert lib.any (lib.hasInfix "${upstreams.go.source_tag}.src.tar.gz") goSourceURLs;
{
  format = formatter.config.build.check inputs.self;

  unit = packages.backup;

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
        nativeBuildInputs = [ pkgs.python3 ];
      }
      ''
        cd ${source}
        python3 -m unittest discover --start-directory tests/scripts --pattern 'test_*.py'
        touch "$out"
      '';

  contracts =
    pkgs.runCommand "vaultwarden-less-contracts"
      {
        nativeBuildInputs = [ pkgs.python3 ];
      }
      ''
        cd ${source}
        python3 - <<'PY'
        import hashlib
        import json
        import re
        import sqlite3
        import tomllib

        with open("UPSTREAMS.toml", "rb") as source:
            upstreams = tomllib.load(source)
        for name, section in upstreams.items():
            assert set(section) == ({
                "repository", "source_tag", "source_commit", "audit_paths"
            } | ({"database_migration", "schema_contract", "recovery_projection"} if name == "vaultwarden" else set()))
            assert re.fullmatch(r"[0-9a-f]{40}", section["source_commit"])
            assert section["repository"].startswith("https://")
            assert section["source_tag"]
            assert section["audit_paths"]

        with open(upstreams["vaultwarden"]["schema_contract"], encoding="utf-8") as source:
            schema = json.load(source)
        with open(upstreams["vaultwarden"]["recovery_projection"], encoding="utf-8") as source:
            projection = json.load(source)
        assert schema["contract_version"] == 1
        assert schema["vaultwarden_version"] == upstreams["vaultwarden"]["source_tag"]
        assert schema["database_migration"] == upstreams["vaultwarden"]["database_migration"]
        digest = hashlib.sha256()
        for item in schema["objects"]:
            for field in ("type", "name", "table", "sql"):
                digest.update(item[field].encode())
                digest.update(b"\0")
            digest.update(b"\n")
        assert schema["fingerprint"] == "sha256:" + digest.hexdigest()

        database = sqlite3.connect(":memory:")
        for item in schema["objects"]:
            if item["type"] == "table":
                database.execute(item["sql"])
        actual = {
            row[0]: [column[1] for column in database.execute(
                "PRAGMA table_info(" + json.dumps(row[0]) + ")"
            )]
            for row in database.execute(
                "SELECT name FROM sqlite_schema WHERE type='table'"
            )
        }
        declared = {table["name"]: table["columns"] for table in projection["tables"]}
        assert actual == declared
        assert projection["schema_version"] == 1
        assert projection["schema_contract"] == upstreams["vaultwarden"]["schema_contract"]
        PY
        touch "$out"
      '';

  toolchain =
    pkgs.runCommand "vaultwarden-less-toolchain-wiring"
      {
        nativeBuildInputs = [
          pkgs.go
          pkgs.restic
          pkgs.docker
          pkgs.docker-compose
          pkgs.golangci-lint
          pkgs.govulncheck
          pkgs.python3
          pkgs.ruff
          pkgs.bitwarden-cli
          pkgs.vaultwarden
        ];
        GOTOOLCHAIN = "local";
        CGO_ENABLED = "1";
      }
      ''
        export HOME="$TMPDIR/home"
        export BITWARDENCLI_APPDATA_DIR="$TMPDIR/bitwarden-cli"
        mkdir -m 0700 "$HOME" "$BITWARDENCLI_APPDATA_DIR"
        test "$(go version)" = "go version go${pkgs.go.version} linux/amd64"
        test "$(go env GOTOOLCHAIN)" = "local"
        test "$(restic version | cut -d' ' -f2)" = "${pkgs.restic.version}"
        test "$(bw --version)" = "${pkgs.bitwarden-cli.version}"
        test -x "$(command -v vaultwarden)"
        test -x "$(command -v docker)"
        test -x "$(command -v govulncheck)"
        touch "$out"
      '';
}
