# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Validate clean HEAD B, then write exact candidate evidence.
check-release:
    nix develop --command python3 scripts/check_release.py

# Export the exact Nix-built HEAD B release after candidate validation.
release output_directory:
    nix develop --command python3 scripts/release.py "{{ output_directory }}"
