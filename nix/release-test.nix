{
  goModules,
  identity,
  motoImage,
  pkgs,
  releaseBundle,
  source,
}:

let
  lib = pkgs.lib;

  e2eTest = pkgs.stdenv.mkDerivation {
    pname = "vaultwarden-less-release-e2e";
    version = identity.release;
    src = source;

    nativeBuildInputs = [
      pkgs.gcc
      pkgs.go
    ];

    GOTOOLCHAIN = "local";
    CGO_ENABLED = "1";
    GOFLAGS = "-mod=vendor";
    GOPROXY = "off";
    GOSUMDB = "off";

    dontConfigure = true;

    buildPhase = ''
      runHook preBuild

      export GOCACHE="$TMPDIR/go-cache"
      export GOMODCACHE="$TMPDIR/go-modules"
      export XDG_CACHE_HOME="$TMPDIR/xdg-cache"
      mkdir -m 0700 "$GOCACHE" "$GOMODCACHE" "$XDG_CACHE_HOME"

      test ! -e vendor
      cp -R --no-preserve=mode ${goModules} vendor
      chmod -R u+w vendor
      go test -c -o vaultwarden-less-e2e ./tests/e2e

      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall
      install -D -m 0755 vaultwarden-less-e2e "$out/bin/vaultwarden-less-e2e"
      install -D -m 0644 compose.yml "$out/share/vaultwarden-less-e2e/compose.yml"
      install -D -m 0644 flake.nix "$out/share/vaultwarden-less-e2e/flake.nix"
      runHook postInstall
    '';
  };

  imageArchives = pkgs.linkFarm "vaultwarden-less-release-test-images-${identity.release}" [
    {
      name = "backup.tar.gz";
      path = "${releaseBundle}/images/backup.tar.gz";
    }
    {
      name = "moto.tar.gz";
      path = motoImage;
    }
    {
      name = "proxy.tar.gz";
      path = "${releaseBundle}/images/proxy.tar.gz";
    }
    {
      name = "vaultwarden.tar.gz";
      path = "${releaseBundle}/images/vaultwarden.tar.gz";
    }
  ];

  e2eCommand = lib.escapeShellArgs [
    "${e2eTest}/bin/vaultwarden-less-e2e"
    "-test.v"
    "-test.run=^(TestTargetlessDeploymentAndFullDataAdoption|TestTwoTargetBackupRestore)$"
    "-test.count=1"
    "-test.timeout=60m"
    "-run-e2e"
    "-e2e-image-directory=${imageArchives}"
    "-e2e-product-image-tag=${identity.release}"
  ];
  testerCommand =
    "cd ${e2eTest}/share/vaultwarden-less-e2e && "
    + "exec env HOME=/home/tester TMPDIR=/home/tester/tmp "
    + "DOCKER_HOST=unix:///var/run/docker.sock ${e2eCommand}";
  runAsTester = "sudo -u tester -H ${lib.getExe pkgs.bash} -c ${lib.escapeShellArg testerCommand}";
in
pkgs.testers.runNixOSTest {
  name = "vaultwarden-less-release-${identity.release}";
  globalTimeout = 75 * 60;

  requiredFeatures = {
    kvm = true;
    nixos-test = true;
  };
  qemu.forceAccel = true;

  nodes.machine =
    { pkgs, ... }:
    {
      virtualisation = {
        cores = 4;
        diskSize = 16 * 1024;
        memorySize = 6 * 1024;
        docker = {
          enable = true;
          package = pkgs.docker;
        };
      };

      users.users.tester = {
        isNormalUser = true;
        uid = 1000;
        group = "users";
        home = "/home/tester";
        createHome = true;
        extraGroups = [ "docker" ];
      };

      environment.systemPackages = [
        pkgs.bitwarden-cli
        pkgs.docker-compose
      ];

      system.extraDependencies = [
        e2eTest
        imageArchives
        releaseBundle
      ];
    };

  testScript = ''
    from datetime import timedelta

    start_all()
    machine.wait_for_unit("docker.service")
    machine.succeed("install -d -o tester -g users -m 0700 /home/tester/tmp")
    machine.succeed("sudo -u tester -H docker info")
    machine.succeed("sudo -u tester -H docker compose version")
    machine.succeed("cd ${releaseBundle} && sha256sum --check SHA256SUMS")
    print(machine.succeed(${builtins.toJSON runAsTester}, timeout=timedelta(minutes=65)))
  '';
}
