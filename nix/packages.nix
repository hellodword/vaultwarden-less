{
  identity,
  pkgs,
  source,
}:
let
  lib = pkgs.lib;
  productVendorHash = "sha256-ZeG8rqE7j7NeJlZB2X1c6JXSyNEl8DIYgwn347lbseQ=";
  product =
    name: subPackage:
    pkgs.buildGoModule {
      pname = name;
      version = identity.release;
      src = source;
      vendorHash = productVendorHash;
      subPackages = [ subPackage ];
      doCheck = false;
      env.CGO_ENABLED = "0";
      ldflags = [
        "-s"
        "-w"
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.ProjectCommit=${identity.projectCommit}"
        "-X=github.com/hellodword/vaultwarden-less/internal/buildinfo.Release=${identity.release}"
      ];
      postInstall = ''
        mkdir -p "$out/share/licenses/vaultwarden-less"
        cp ${source}/LICENSE ${source}/NOTICE.md "$out/share/licenses/vaultwarden-less/"
      '';
      meta = {
        license = lib.licenses.mit;
        platforms = [ "x86_64-linux" ];
      };
    };
  backup = product "vaultwarden-less-backup" "cmd/vaultwarden-less-backup";
  proxy = product "vaultwarden-less-proxy" "cmd/vaultwarden-less-proxy";
  labels =
    title:
    {
      "org.opencontainers.image.title" = title;
      "org.opencontainers.image.version" = identity.release;
      "org.opencontainers.image.source" = "https://github.com/hellodword/vaultwarden-less";
    }
    // lib.optionalAttrs (identity.projectCommit != "") {
      "org.opencontainers.image.revision" = identity.projectCommit;
    };
  backupImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-backup";
    tag = identity.release;
    created = identity.created;
    contents = [
      backup
      pkgs.restic
      pkgs.sqlite
      pkgs.cacert
      pkgs.tzdata
    ];
    config = {
      WorkingDir = "/";
      Entrypoint = [ "/bin/vaultwarden-less-backup" ];
      Cmd = [ "serve" ];
      Labels = labels "Vaultwarden native backup";
    };
  };
  proxyImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-proxy";
    tag = identity.release;
    created = identity.created;
    contents = [ proxy ];
    config = {
      WorkingDir = "/";
      Entrypoint = [ "/bin/vaultwarden-less-proxy" ];
      Cmd = [ "serve" ];
      Labels = labels "Vaultwarden request proxy";
    };
  };
  vaultwardenHealthcheck = pkgs.writeShellApplication {
    name = "healthcheck.sh";
    runtimeInputs = [ pkgs.curl ];
    text = "exec curl --fail --silent --show-error --max-time 3 http://127.0.0.1/alive";
  };
  vaultwardenImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-vaultwarden";
    tag = identity.release;
    created = identity.created;
    contents = [
      pkgs.vaultwarden
      vaultwardenHealthcheck
      pkgs.cacert
      pkgs.tzdata
    ];
    extraCommands = "ln -s ${vaultwardenHealthcheck}/bin/healthcheck.sh healthcheck.sh";
    config = {
      WorkingDir = "/";
      Entrypoint = [ "/bin/vaultwarden" ];
      Env = [
        "ROCKET_ADDRESS=0.0.0.0"
        "ROCKET_PORT=80"
      ];
      ExposedPorts."80/tcp" = { };
      Labels = labels "Vaultwarden";
    };
  };
  releaseBundle = pkgs.runCommand "vaultwarden-less-release-${identity.release}" { } ''
    mkdir -p "$out/images" "$out/templates" "$out/docs"
    cp ${backupImage} "$out/images/backup.tar.gz"
    cp ${proxyImage} "$out/images/proxy.tar.gz"
    cp ${vaultwardenImage} "$out/images/vaultwarden.tar.gz"
    cp ${source}/compose.yml ${source}/LICENSE ${source}/NOTICE.md ${source}/README.md "$out/"
    cp -R ${source}/templates/. "$out/templates/"
    cp -R ${source}/docs/. "$out/docs/"
    substituteInPlace "$out/templates/compose.env" --replace-fail 'VWLESS_IMAGE_TAG=development' 'VWLESS_IMAGE_TAG=${identity.release}'
  '';
  motoPython = pkgs.python3.withPackages (p: [ p.moto ] ++ p.moto.optional-dependencies.server);
  motoImage = pkgs.dockerTools.buildLayeredImage {
    name = "vaultwarden-less-moto";
    tag = "local";
    created = identity.created;
    contents = [
      motoPython
      pkgs.cacert
    ];
    config = {
      WorkingDir = "/";
      Entrypoint = [ "/bin/moto_server" ];
      Cmd = [
        "-H"
        "0.0.0.0"
      ];
    };
  };
  testRunnerImage = import ./release-test.nix {
    inherit
      pkgs
      source
      identity
      motoImage
      ;
    goModules = backup.goModules;
  };
in
{
  inherit backup proxy;
  backup-image = backupImage;
  proxy-image = proxyImage;
  vaultwarden-image = vaultwardenImage;
  moto-image = motoImage;
  release = releaseBundle;
  test-runner-image = testRunnerImage;
}
