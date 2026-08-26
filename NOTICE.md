# vaultwarden-less notices

This notice is maintained by hand for the binaries and runtime files shipped
in the vaultwarden-less 0.1.0 Linux/amd64 image. Review it whenever a Go
module, Restic, Go builder, or runtime base image changes.

## Product

- vaultwarden-less is distributed under the MIT license in `/licenses/vaultwarden-less/LICENSE`.
- The product is built with Go 1.26.7 using `golang:1.26.7-trixie` and runs on
  `gcr.io/distroless/base-debian13:nonroot` at the exact digests in the
  Dockerfile. The builder is not copied into the final image. Go's license is
  in `third-party/go-stdlib-BSD-3-Clause.txt`; Distroless is covered by
  `third-party/Apache-2.0.txt`. Debian runtime components retain their
  individual upstream licensing.

## Linked Go components

- `github.com/goccy/go-yaml` v1.19.2 — MIT;
  `third-party/goccy-go-yaml-MIT.txt`.
- `github.com/mattn/go-sqlite3` v1.14.50 — MIT;
  `third-party/mattn-go-sqlite3-MIT.txt`. Its embedded SQLite source is public
  domain; see `third-party/sqlite-public-domain.txt`.
- `github.com/spf13/cobra` v1.10.2 — Apache-2.0;
  `third-party/Apache-2.0.txt`.
- `github.com/spf13/pflag` v1.0.9 — BSD-3-Clause;
  `third-party/spf13-pflag-BSD-3-Clause.txt`.

## Bundled executable

- Restic 0.19.1 is copied from the exact `ghcr.io/restic/restic:0.19.1`
  image digest in the Dockerfile and is distributed under BSD-2-Clause; see
  `third-party/restic-BSD-2-Clause.txt`.
