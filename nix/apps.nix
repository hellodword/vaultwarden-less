{
  packages,
  pkgs,
  source,
}:
let
  app = name: runtimeInputs: text: {
    type = "app";
    meta.description = "Run the ${name} workflow with pinned tools and inputs";
    program = "${pkgs.writeShellApplication { inherit name runtimeInputs text; }}/bin/${name}";
  };
  baselines = {
    vaultwarden = pkgs.vaultwarden.src;
    restic = pkgs.restic.src;
    bitwarden-cli = pkgs.bitwarden-cli.src;
    moto = pkgs.python3Packages.moto.src;
    go = pkgs.go.src;
  };
in
{
  check-release = app "check-release" [ pkgs.python3 pkgs.docker pkgs.docker-compose ] ''
    exec python3 ${source}/scripts/check_release.py \
      --release-dir ${packages.release} \
      --runner-archive ${packages.test-runner-image} \
      --runner-image ${pkgs.lib.escapeShellArg "${packages.test-runner-image.imageName}:${packages.test-runner-image.imageTag}"} \
      --work-dir "$PWD/tmp/agent/release-tests" \
      --docker-socket /var/run/docker.sock "$@"
  '';
  routes = app "routes" [ pkgs.python3 ] ''
    exec python3 ${source}/scripts/routes.py ${pkgs.vaultwarden.src} \
      --output "$PWD/internal/routes/catalog.txt" "$@"
  '';
}
// pkgs.lib.mapAttrs' (
  component: baseline:
  pkgs.lib.nameValuePair "upstream-diff-${component}" (
    app "upstream-diff-${component}" [ pkgs.python3 pkgs.git ] ''
      exec python3 ${source}/scripts/upstream_diff.py ${component} \
        --baseline ${baseline} --work-dir "$PWD/tmp/agent/upstream-audit" "$@"
    ''
  )
) baselines
