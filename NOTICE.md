# vaultwarden-less notices

This notice describes the production binaries and images created by the Nix
flake and the offline bundle. Exact product and upstream revisions are recorded
in `VERSION`, `UPSTREAMS.toml`, `flake.lock`, `go.mod`, and `go.sum`.
The bundle's CycloneDX documents are the exact image-level inventory for a
particular build.

Review this file whenever an upstream, Go module, Nix input, image composition,
or distributed license changes.

## Product

Vaultwarden-less is distributed under the MIT license in `LICENSE`. The
backup and proxy binaries are built by Nix without a separate container-build
definition or runtime base image. Nix `dockerTools` assembles only the declared
binary and runtime closures.

The Go standard-library license is reproduced in
`licenses/third-party/go-stdlib-BSD-3-Clause.txt`.

## Linked Go components

The authoritative versions are in `go.mod` and `go.sum`.

- `github.com/goccy/go-yaml` — MIT;
  `licenses/third-party/goccy-go-yaml-MIT.txt`.
- `github.com/mattn/go-sqlite3` — MIT;
  `licenses/third-party/mattn-go-sqlite3-MIT.txt`. Its embedded SQLite source
  is public domain; see
  `licenses/third-party/sqlite-public-domain.txt`.
- `github.com/spf13/cobra` — Apache-2.0;
  `licenses/third-party/Apache-2.0.txt`.
- `github.com/spf13/pflag` — BSD-3-Clause;
  `licenses/third-party/spf13-pflag-BSD-3-Clause.txt`.

Transitive and Nix runtime components remain subject to their own upstream
licenses and are enumerated in the generated SBOMs.

## Locally built Restic

Restic is built locally from the exact upstream commit in `UPSTREAMS.toml`
with `nix/patches/restic-go-1.26.patch`. The patch and its pinned source hash
are part of the bundled project source. Restic is BSD-2-Clause; the notice is
reproduced in `licenses/third-party/restic-BSD-2-Clause.txt` and copied into
the backup image.

The upstream project and license are:
https://github.com/restic/restic and
https://github.com/restic/restic/blob/master/LICENSE.

## Pinned Vaultwarden image

The offline bundle includes the unmodified official Vaultwarden image selected
by the exact repository digest in `UPSTREAMS.toml`, retagged only with a local
name. Vaultwarden declares `AGPL-3.0-only`. The corresponding exact source
commit is recorded in `UPSTREAMS.toml`; source and license are available at:

- https://github.com/dani-garcia/vaultwarden
- https://github.com/dani-garcia/vaultwarden/blob/main/LICENSE.txt

Do not replace the digest with a floating tag. Recheck the image contents,
license, and corresponding source whenever Vaultwarden is upgraded.

## Test-only tools

Moto and the official Bitwarden CLI are development/E2E tools supplied through
Nix. The Moto image is built locally for tests and is not included in the
production offline bundle. Neither tool is copied into the backup or proxy
image.
