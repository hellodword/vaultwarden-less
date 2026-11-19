{
  identity,
  pkgs,
  source,
}:
let
  lib = pkgs.lib;
  productVendorHash = "sha256-NwNhfVDicjkKenOTbTVksU4vvDrD6yWEx2RQhFQ+bxc=";
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
      pkgs.coreutils
      pkgs.sqlite
      pkgs.cacert
      pkgs.tzdata
    ];
    config = {
      WorkingDir = "/";
      User = "1000:1000";
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
      User = "1000:1000";
      Entrypoint = [ "/bin/vaultwarden-less-proxy" ];
      Cmd = [ "serve" ];
      Labels = labels "Vaultwarden request proxy";
    };
  };
  vaultwardenImage = pkgs.dockerTools.pullImage {
    imageName = "vaultwarden/server";
    finalImageTag = "1.37.5";
    imageDigest = "sha256:f8863ab2519364bfc657144006682a830784e0bb501606d5e34516ee76b60b5c";
    hash = "sha256-dBh6VRIA2QVo8thC/g52Lh33Hl4VhZqrw4uG6RHYTno=";
    os = "linux";
    arch = "amd64";
  };
  releaseBundle = pkgs.runCommand "vaultwarden-less-release-${identity.release}" { } ''
    mkdir -p "$out/images" "$out/templates" "$out/docs"
    cp ${backupImage} "$out/images/backup.tar.gz"
    cp ${proxyImage} "$out/images/proxy.tar.gz"
    gzip -n -c ${vaultwardenImage} > "$out/images/vaultwarden.tar.gz"
    cp ${source}/compose.yml ${source}/LICENSE ${source}/NOTICE.md ${source}/README.md "$out/"
    cp -R ${source}/templates/. "$out/templates/"
    cp -R ${source}/rules "$out/rules"
    cp -R ${source}/docs/. "$out/docs/"
    for image in vaultwarden-less-backup vaultwarden-less-proxy; do
      substituteInPlace "$out/compose.yml" --replace-fail "$image:development" "$image:${identity.release}"
      substituteInPlace "$out"/docs/*.md --replace-quiet "$image:development" "$image:${identity.release}"
    done
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
