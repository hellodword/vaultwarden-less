# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Run the full optional release checks for one unchanged clean commit.
check-release:
    nix build --no-link -L .#release-test

# Build the immutable deployment bundle.
release:
    nix build .#release
