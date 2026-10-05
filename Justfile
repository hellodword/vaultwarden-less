# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Build the runner and execute the same suite on the current Docker host.
check-release *args:
    nix run .#check-release -- "$@"

# Build the deployment bundle.
release:
    nix build .#release

# Regenerate the route catalog from the selected Vaultwarden source.
routes *args:
    nix run .#routes -- "$@"

# Compare a candidate Git ref with the selected upstream source.
upstream-diff component ref *args:
    #!/usr/bin/env bash
    set -euo pipefail
    component=$1
    shift
    exec nix run ".#upstream-diff-$component" -- "$@"
set positional-arguments
