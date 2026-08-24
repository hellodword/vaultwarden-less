# Third-party software inventory

The project images contain the following third-party software. This inventory is
part of the image at `/licenses/THIRD_PARTY_LICENSES.md`; image publication must
also preserve the license files supplied by each base image.

| Component | Version | License | Source |
| --- | --- | --- | --- |
| Go | 1.26.7 | BSD-3-Clause | <https://go.dev/LICENSE> |
| Cobra | 1.10.2 | Apache-2.0 | <https://github.com/spf13/cobra> |
| pflag | 1.0.9 | BSD-3-Clause | <https://github.com/spf13/pflag> |
| goccy/go-yaml | 1.19.2 | MIT | <https://github.com/goccy/go-yaml> |
| go-sqlite3 | 1.14.50 | MIT | <https://github.com/mattn/go-sqlite3> |
| SQLite amalgamation included by go-sqlite3 | bundled upstream version | Public domain | <https://sqlite.org/copyright.html> |
| Restic | 0.19.1 | BSD-2-Clause | <https://github.com/restic/restic> |
| Distroless | Debian 13 images | Apache-2.0 plus packaged component licenses | <https://github.com/GoogleContainerTools/distroless> |

`github.com/inconshreveable/mousetrap` is recorded in `go.sum` for Cobra's
Windows support but is not linked into the Linux binaries shipped here.
