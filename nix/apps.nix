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
}
