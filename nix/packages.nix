{
  identity,
  pkgs,
  source,
  upstreams,
}:

let
  lib = pkgs.lib;
  productVendorHash = "sha256-kOSaH7fkHW/WUqKKWFU8lqmJsvzW/quxqnLOaorRyI8=";
  cleanString = if identity.clean then "true" else "false";

  productPackage =
    {
      name,
      subPackage,
      cgoEnabled,
      check ? false,
    }:
    pkgs.buildGoModule {
      pname = name;
      version = identity.release;
      src = source;
      vendorHash = productVendorHash;
      subPackages = [ subPackage ];
      doCheck = check;
      env.CGO_ENABLED = if cgoEnabled then "1" else "0";
      ldflags = [
        "-s"
        "-w"
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.ProjectCommit=${identity.projectCommit}"
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.Release=${identity.release}"
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.Clean=${cleanString}"
      ];
      postInstall = ''
        mkdir -p "$out/share/licenses/vaultwarden-less"
        cp ${source}/LICENSE "$out/share/licenses/vaultwarden-less/LICENSE"
        cp ${source}/NOTICE.md "$out/share/licenses/vaultwarden-less/NOTICE.md"
      '';
      meta = {
        license = lib.licenses.mit;
        platforms = [ "x86_64-linux" ];
      };
    };

  backup = productPackage {
    name = "vaultwarden-less-backup";
    subPackage = "cmd/vaultwarden-less-backup";
    cgoEnabled = true;
    check = true;
  };

  proxy = productPackage {
    name = "vaultwarden-less-proxy";
    subPackage = "cmd/vaultwarden-less-proxy";
    cgoEnabled = false;
  };

  labels =
    title:
    {
      "org.opencontainers.image.title" = title;
      "org.opencontainers.image.version" = identity.release;
      "org.opencontainers.image.created" = identity.created;
      "org.opencontainers.image.source" = "https://github.com/hellodword/vaultwarden-less";
    }
    // lib.optionalAttrs identity.clean {
      "org.opencontainers.image.revision" = identity.projectCommit;
    };

  commonImageConfig = {
    WorkingDir = "/";
  };

  backupImage =
    tag:
    pkgs.dockerTools.buildLayeredImage {
      name = "vaultwarden-less-backup";
      inherit tag;
      created = identity.created;
      contents = [
        backup
        pkgs.restic
        pkgs.cacert
        pkgs.tzdata
      ];
      config = commonImageConfig // {
        Entrypoint = [ "/bin/vaultwarden-less-backup" ];
        Cmd = [ "serve" ];
        Labels = labels "vaultwarden-less backup";
      };
    };

  proxyImage =
    tag:
    pkgs.dockerTools.buildLayeredImage {
      name = "vaultwarden-less-proxy";
      inherit tag;
      created = identity.created;
      contents = [ proxy ];
      config = commonImageConfig // {
        Entrypoint = [ "/bin/vaultwarden-less-proxy" ];
        Cmd = [ "serve" ];
        Labels = labels "vaultwarden-less proxy";
      };
    };

  vaultwardenHealthcheck = pkgs.writeShellApplication {
    name = "healthcheck.sh";
    runtimeInputs = [ pkgs.curl ];
    text = ''
      exec curl --fail --silent --show-error --max-time 3 http://127.0.0.1/alive
    '';
  };

  vaultwardenImage =
    tag:
    pkgs.dockerTools.buildLayeredImage {
      name = "vaultwarden-less-vaultwarden";
      inherit tag;
      created = identity.created;
      extraCommands = ''
        ln -s ${vaultwardenHealthcheck}/bin/healthcheck.sh healthcheck.sh
      '';
      contents = [
        pkgs.vaultwarden
        vaultwardenHealthcheck
        pkgs.cacert
        pkgs.tzdata
      ];
      config = commonImageConfig // {
        Entrypoint = [ "/bin/vaultwarden" ];
        Env = [
          "ROCKET_ADDRESS=0.0.0.0"
          "ROCKET_PORT=80"
        ];
        ExposedPorts = {
          "80/tcp" = { };
        };
        Labels = labels "vaultwarden-less Vaultwarden";
      };
    };

  releaseImages = {
    backup = backupImage identity.release;
    proxy = proxyImage identity.release;
    vaultwarden = vaultwardenImage identity.release;
  };

  releaseManifest = pkgs.writeText "vaultwarden-less-release.json" (
    builtins.toJSON {
      format = 1;
      commit = identity.projectCommit;
      release = identity.release;
      system = pkgs.stdenv.hostPlatform.system;
      images = [
        {
          archive = "images/backup.tar.gz";
          reference = "vaultwarden-less-backup:${identity.release}";
        }
        {
          archive = "images/proxy.tar.gz";
          reference = "vaultwarden-less-proxy:${identity.release}";
        }
        {
          archive = "images/vaultwarden.tar.gz";
          reference = "vaultwarden-less-vaultwarden:${identity.release}";
        }
      ];
    }
    + "\n"
  );

  releaseBundle =
    if identity.clean then
      pkgs.runCommand "vaultwarden-less-release-${identity.release}" { } ''
        mkdir -p \
          "$out/images" \
          "$out/config" \
          "$out/credentials/restic" \
          "$out/examples/targetless/config" \
          "$out/examples/targetless/credentials" \
          "$out/internal/upstream"

        install -m 0644 ${releaseImages.backup} "$out/images/backup.tar.gz"
        install -m 0644 ${releaseImages.proxy} "$out/images/proxy.tar.gz"
        install -m 0644 ${releaseImages.vaultwarden} "$out/images/vaultwarden.tar.gz"
        install -m 0644 ${releaseManifest} "$out/release.json"
        install -m 0644 ${source}/RELEASE.md "$out/RELEASE.md"
        install -m 0644 ${source}/compose.yml "$out/compose.yml"
        substitute ${source}/.env.example "$out/.env.example" \
          --replace-fail 'VWLESS_IMAGE_TAG=local' 'VWLESS_IMAGE_TAG=${identity.release}'

        install -m 0644 ${source}/config/backup.example.yaml "$out/config/backup.example.yaml"
        install -m 0644 ${source}/config/vaultwarden.example.env "$out/config/vaultwarden.example.env"
        install -m 0644 ${source}/credentials/backup.example.yaml "$out/credentials/backup.example.yaml"
        install -m 0644 ${source}/credentials/maintenance.example.yaml "$out/credentials/maintenance.example.yaml"
        install -m 0644 ${source}/credentials/notifiers.example.yaml "$out/credentials/notifiers.example.yaml"
        install -m 0644 ${source}/credentials/restic/primary.example "$out/credentials/restic/primary.example"

        install -m 0644 ${source}/examples/targetless/config/backup.yaml "$out/examples/targetless/config/backup.yaml"
        install -m 0644 ${source}/examples/targetless/credentials/backup.yaml "$out/examples/targetless/credentials/backup.yaml"
        install -m 0644 ${source}/examples/targetless/credentials/maintenance.yaml "$out/examples/targetless/credentials/maintenance.yaml"
        install -m 0644 ${source}/examples/targetless/credentials/notifiers.yaml "$out/examples/targetless/credentials/notifiers.yaml"

        cp -R --no-preserve=mode ${source}/docs "$out/docs"
        install -m 0644 ${source}/UPSTREAMS.toml "$out/UPSTREAMS.toml"
        install -m 0644 ${source}/internal/upstream/vaultwarden-schema.json "$out/internal/upstream/vaultwarden-schema.json"
        install -m 0644 ${source}/internal/upstream/recovery-projection.json "$out/internal/upstream/recovery-projection.json"
        install -m 0644 ${source}/LICENSE "$out/LICENSE"
        install -m 0644 ${source}/NOTICE.md "$out/NOTICE.md"

        find "$out" -type d -exec chmod 0755 {} +
        find "$out" -type f -exec chmod 0644 {} +
        cd "$out"
        find . -type f ! -path './SHA256SUMS' -printf '%P\0' \
          | LC_ALL=C sort -z \
          | xargs -0 sha256sum -- > SHA256SUMS
        chmod 0644 SHA256SUMS
      ''
    else
      pkgs.runCommand "vaultwarden-less-release-requires-clean-git" { } ''
        echo "packages.release requires a clean Git revision" >&2
        exit 1
      '';

  motoPython = pkgs.python3.withPackages (
    pythonPackages: [ pythonPackages.moto ] ++ pythonPackages.moto.optional-dependencies.server
  );

  motoImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-moto";
    tag = "local";
    created = "1970-01-01T00:00:01Z";
    contents = [
      motoPython
      pkgs.cacert
    ];
    config = commonImageConfig // {
      Entrypoint = [ "/bin/moto_server" ];
      Cmd = [
        "-H"
        "0.0.0.0"
      ];
      Labels = {
        "org.opencontainers.image.title" = "vaultwarden-less test Moto";
        "org.opencontainers.image.version" = upstreams.moto.source_tag;
        "org.opencontainers.image.created" = "1970-01-01T00:00:01Z";
      };
    };
  };
in
{
  inherit backup proxy;
  backup-image = backupImage "local";
  proxy-image = proxyImage "local";
  release = releaseBundle;
  vaultwarden-image = vaultwardenImage "local";
  moto-image = motoImage;
}
