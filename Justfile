# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Run the full optional release checks for one unchanged clean commit.
check-release:
    nix develop --command python3 -B scripts/check_release.py

# Build the immutable deployment bundle and print its Nix store path.
release:
    @nix build --no-link --print-out-paths .#release
