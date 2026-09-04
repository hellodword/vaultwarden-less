# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Run the full optional release checks for one unchanged clean commit.
check-release:
    nix develop --command go test -v ./tests/e2e -run '^(TestTargetlessDeploymentAndFullDataAdoption|TestTwoTargetBackupRestore)$' -count=1 -timeout=60m -args -run-e2e

# Build the immutable deployment bundle.
release:
    nix build .#release
