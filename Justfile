# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Build the runner and execute the same suite on the current Docker host.
check-release:
    python3 scripts/check_release.py

# Build the deployment bundle.
release:
    nix build .#release
