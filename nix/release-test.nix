{
  goModules,
  identity,
  motoImage,
  pkgs,
  source,
}:
let
  tests = pkgs.stdenv.mkDerivation {
    pname = "vaultwarden-less-tests";
    version = identity.release;
    src = source;
    nativeBuildInputs = [ pkgs.go ];
    GOTOOLCHAIN = "local";
    CGO_ENABLED = "0";
    GOFLAGS = "-mod=vendor";
    GOPROXY = "off";
    GOSUMDB = "off";
    dontConfigure = true;
    buildPhase = ''
      export GOCACHE="$TMPDIR/go-cache" GOMODCACHE="$TMPDIR/go-modules"
      cp -R --no-preserve=mode ${goModules} vendor
      go test -c -o vaultwarden-less-tests ./tests/e2e
    '';
    installPhase = ''
      install -D -m 0755 vaultwarden-less-tests "$out/bin/vaultwarden-less-tests"
    '';
  };
  runner = pkgs.writeShellScriptBin "test-runner" ''
    exec ${tests}/bin/vaultwarden-less-tests -test.v -test.timeout=75m -run-e2e "$@"
  '';
  motoArchive = pkgs.runCommand "vaultwarden-less-moto-archive" { } ''
    mkdir -p "$out/share"
    ln -s ${motoImage} "$out/share/moto.tar.gz"
  '';
in
pkgs.dockerTools.buildLayeredImage {
  name = "vaultwarden-less-test-runner";
  tag = "local";
  created = identity.created;
  contents = [
    tests
    runner
    motoArchive
    pkgs.bitwarden-cli
    pkgs.docker
    pkgs.docker-compose
    pkgs.restic
    pkgs.sqlite
    pkgs.cacert
    pkgs.coreutils
    pkgs.bash
  ];
  extraCommands = ''
    mkdir -p tmp
    chmod 1777 tmp
  '';
  config = {
    WorkingDir = "/";
    Entrypoint = [ "/bin/test-runner" ];
  };
}
