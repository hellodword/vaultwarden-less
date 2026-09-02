# vaultwarden-less notices

Vaultwarden-less is distributed under the MIT license in `LICENSE`. The
authoritative project and Go dependency versions are recorded in `go.mod`,
`go.sum`, `UPSTREAMS.toml`, and `flake.lock`. Nix builds the project binaries
and assembles the runtime images from the closures selected by that lock.

This project does not claim that its deployment bundle is a complete
software bill of materials, license archive, or source archive. Every linked
or included component remains subject to its own license. Review those
licenses and the locked dependency graph before redistributing an image
release.

## Project binaries

The backup and proxy binaries are MIT-licensed. Their linked Go dependencies
include components under MIT, BSD, Apache-2.0, and public-domain terms; the
exact module graph is defined by `go.mod` and `go.sum`. Nixpkgs retains its
normal package metadata and vulnerability policy for the selected packages.

## Restic

The backup image includes the unmodified `pkgs.restic` selected by the locked
nixpkgs input. Restic is distributed under the BSD 2-Clause license. Its
project, source, and license are available at:

- https://github.com/restic/restic
- https://github.com/restic/restic/blob/master/LICENSE

## Vaultwarden

The Vaultwarden production image is assembled from `pkgs.vaultwarden` at the
audited tag and commit recorded in `UPSTREAMS.toml`. Vaultwarden is licensed
under AGPL-3.0-only. Anyone distributing the resulting Vaultwarden image must
satisfy the AGPL's corresponding-source and notice obligations; the
vaultwarden-less deployment bundle does not itself contain that source. The
audited upstream source and license are available at:

- https://github.com/dani-garcia/vaultwarden
- https://github.com/dani-garcia/vaultwarden/blob/main/LICENSE.txt

## Test-only tools

Moto and the official Bitwarden CLI are development and E2E tools supplied by
the locked Nix environment. The Moto image is test-only and is excluded from
the production image release. Neither tool is copied into the backup or proxy
image.
