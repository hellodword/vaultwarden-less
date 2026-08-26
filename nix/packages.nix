{
  pkgs,
  go,
  source,
  upstreams,
  productVersion,
}:

let
  lib = pkgs.lib;
  buildGoModule = pkgs.buildGoModule.override { inherit go; };
  productVendorHash = "sha256-kOSaH7fkHW/WUqKKWFU8lqmJsvzW/quxqnLOaorRyI8=";

  productPackage =
    {
      name,
      subPackage,
      cgoEnabled,
      check ? false,
    }:
    buildGoModule {
      pname = name;
      version = productVersion;
      src = source;
      vendorHash = productVendorHash;
      subPackages = [ subPackage ];
      doCheck = check;
      env.CGO_ENABLED = if cgoEnabled then "1" else "0";
      ldflags = [
        "-s"
        "-w"
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.Version=${productVersion}"
      ];
      postInstall = ''
        mkdir -p "$out/share/licenses/vaultwarden-less"
        cp ${source}/LICENSE "$out/share/licenses/vaultwarden-less/LICENSE"
        cp ${source}/NOTICE.md "$out/share/licenses/vaultwarden-less/NOTICE.md"
        cp -R ${source}/licenses/third-party "$out/share/licenses/vaultwarden-less/third-party"
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

  resticPatched = buildGoModule {
    pname = "vaultwarden-less-restic";
    version = upstreams.restic.version;
    src = pkgs.fetchFromGitHub {
      owner = "restic";
      repo = "restic";
      rev = upstreams.restic.source_commit;
      hash = upstreams.restic.source_hash;
    };
    patches = [ ./patches/restic-go-1.26.patch ];
    vendorHash = upstreams.restic.vendor_hash;
    subPackages = [ "cmd/restic" ];
    doCheck = false;
    env.CGO_ENABLED = "0";
    postPatch = ''
      rm cmd/restic/cmd_mount_integration_test.go
    '';
    postInstall = ''
      mkdir -p "$out/share/licenses/restic"
      cp LICENSE "$out/share/licenses/restic/LICENSE"
    '';
    meta = {
      license = lib.licenses.bsd2;
      mainProgram = "restic";
      platforms = [ "x86_64-linux" ];
    };
  };

  motoPython =
    assert pkgs.python3Packages.moto.version == upstreams.moto.version;
    pkgs.python3.withPackages (
      pythonPackages: [ pythonPackages.moto ] ++ pythonPackages.moto.optional-dependencies.server
    );

  commonImageConfig = {
    WorkingDir = "/";
  };

  backupImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-backup";
    tag = productVersion;
    created = "1970-01-01T00:00:01Z";
    contents = [
      backup
      resticPatched
      pkgs.cacert
      pkgs.tzdata
    ];
    config = commonImageConfig // {
      Entrypoint = [ "/bin/vaultwarden-less-backup" ];
      Cmd = [
        "serve"
        "--config"
        "/config/backup.yaml"
      ];
      Labels = {
        "org.opencontainers.image.title" = "vaultwarden-less backup";
        "org.opencontainers.image.version" = productVersion;
        "org.opencontainers.image.source" = "local Nix build";
      };
    };
  };

  proxyImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-proxy";
    tag = productVersion;
    created = "1970-01-01T00:00:01Z";
    contents = [ proxy ];
    config = commonImageConfig // {
      Entrypoint = [ "/bin/vaultwarden-less-proxy" ];
      Cmd = [ "serve" ];
      Labels = {
        "org.opencontainers.image.title" = "vaultwarden-less proxy";
        "org.opencontainers.image.version" = productVersion;
        "org.opencontainers.image.source" = "local Nix build";
      };
    };
  };

  motoImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-moto";
    tag = upstreams.moto.version;
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
        "org.opencontainers.image.version" = upstreams.moto.version;
        "org.opencontainers.image.source" = "local Nix build";
      };
    };
  };
in
{
  inherit backup proxy;
  "restic-patched" = resticPatched;
  "backup-image" = backupImage;
  "proxy-image" = proxyImage;
  "moto-image" = motoImage;
}
