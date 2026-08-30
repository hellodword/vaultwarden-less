# Run every ordinary repository check through the pinned flake.
check:
    nix flake check

# Validate clean A and HEAD B, then write exact candidate evidence.
check-release from_commit:
    nix develop --command python3 scripts/check_release.py "{{ from_commit }}"

# Build and verify the exact two-file HEAD B release for a checked A/B pair.
release from_commit output_directory:
    nix develop --command python3 scripts/release.py "{{ from_commit }}" "{{ output_directory }}"
