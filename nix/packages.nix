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
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.Version=${identity.release}"
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

  drillCopy = productPackage {
    name = "vaultwarden-less-drill-copy";
    subPackage = "cmd/vaultwarden-less-drill-copy";
    cgoEnabled = true;
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

  backupImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-backup";
    tag = "local";
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

  proxyImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-proxy";
    tag = "local";
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

  vaultwardenImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-vaultwarden";
    tag = "local";
    created = identity.created;
    extraCommands = ''
      ln -s ${vaultwardenHealthcheck}/bin/healthcheck.sh healthcheck.sh
    '';
    contents = [
      pkgs.vaultwarden
      drillCopy
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
  backup-image = backupImage;
  proxy-image = proxyImage;
  vaultwarden-image = vaultwardenImage;
  moto-image = motoImage;
}
