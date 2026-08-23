# 1.0.0 security validation record

This is point-in-time evidence for the locally built `linux/amd64` release
candidate on 2026-08-23. It is not a claim that future builds, mutable image
tags, or vulnerability databases will produce the same result.

## Tools and boundaries

- Trivy `0.73.0` scanned OS packages and Go binaries in the final images.
- `govulncheck 1.7.0` used the Go vulnerability database updated at
  `2026-08-21T20:38:00Z` and binary mode for the exact extracted executables.
- Binary-mode `govulncheck` uses symbols present in a binary and cannot recover
  complete call graphs. Its findings may include unreachable code, but they
  must not be described as harmless without a separate reachability review.
  See the [official binary-mode limitations](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck#hdr-Limitations).

The project-owned proxy and backup executables were built with Go `1.26.7`.
Both returned `No vulnerabilities found` from binary-mode `govulncheck`.

The amd64 validation resolved the version tags to these registry digests:

| Tagged input | Resolved digest |
| --- | --- |
| `vaultwarden/server:1.37.2` | `sha256:094b5689ed81549bd293418395c7cf495ae9d960fc2d4928cef2083ef913d912` |
| `golang:1.26.7-trixie` | `sha256:b389f1219965d8ba67776b81d17308ab25fa763be3855e5fe63ebcb10e15f3a1` |
| `gcr.io/distroless/static-debian13:nonroot` | `sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7` |
| `gcr.io/distroless/base-nossl-debian13:nonroot` | `sha256:5cab74e7f8a5e7c5f1c8a9e6268b1f352f053c36c656f493308340bcecbc636c` |
| `ghcr.io/restic/restic:0.19.1` | `sha256:2f0373803493361f9304a57150d464677f69a9dad487afec202105aafb2592f2` |
| `aquasec/trivy:0.73.0` | `sha256:7cced7cae583819fc7806d4cbc0dbbc7cad18b99f7d3e235192e6da8c091045c` |

These are evidence, not deployment pins; Compose and Dockerfiles deliberately
retain the accepted version-tag policy.

## Image results

| Final image component | Trivy result | Attribution |
| --- | ---: | --- |
| proxy image | 0 | No OS-package or Go-binary findings detected at scan time. |
| project backup executable | 0 | No Go-binary findings detected at scan time. |
| backup image Debian `libc6` | 13 | Distroless Debian 13.6 base: 6 medium and 7 low findings; the scanner reported no fixed Debian package version. |
| embedded Restic executable | 14 | Official Restic `0.19.1` image binary: 12 high, 1 medium, and 1 unknown finding. |

The 13 `libc6` records are `CVE-2026-5435`, `CVE-2026-5450`,
`CVE-2026-5928`, `CVE-2026-6238`, `CVE-2026-6368`, `CVE-2026-6791`,
`CVE-2010-4756`, `CVE-2018-20796`, `CVE-2019-1010022`,
`CVE-2019-1010023`, `CVE-2019-1010024`, `CVE-2019-1010025`, and
`CVE-2019-9192`.

The 14 Trivy records attributed to the embedded Restic executable are
`GO-2026-5932`, `CVE-2026-46600` in `x/net`, `CVE-2026-56852`,
`GHSA-hrxh-6v49-42gf`, and ten standard-library records:
`CVE-2026-33818`, `CVE-2026-39821`, `CVE-2026-39822`,
`CVE-2026-46600`, `CVE-2026-56853`, `CVE-2026-56858`,
`CVE-2026-56859`, `CVE-2026-56860`, `CVE-2026-56862`, and
`CVE-2026-42505`.

## Restic binary review

The binary copied from `ghcr.io/restic/restic:0.19.1` reports Go `1.26.4`,
`x/net v0.55.0`, `x/text v0.37.0`, gRPC `v1.81.1`, and `x/crypto v0.52.0`.
Binary-mode `govulncheck` identified these 15 Go vulnerability records by
symbol: `GO-2026-6218`, `GO-2026-6091`, `GO-2026-6090`, `GO-2026-6089`,
`GO-2026-6088`, `GO-2026-6061`, `GO-2026-5972`, `GO-2026-5970`,
`GO-2026-5942`, `GO-2026-5932`, `GO-2026-5856`, `GO-2026-5841`,
`GO-2026-5158`, `GO-2026-5026`, and `GO-2026-4970`.

Most have published fixes in newer Go or module versions. `GO-2026-5932`
instead states that `x/crypto/openpgp` is unmaintained and has no fixed
version. The exact impact depends on which Restic command and backend makes a
symbol reachable; this validation did not dismiss any finding as unreachable.

The v1 image contract intentionally copies the official Restic `0.19.1`
binary rather than maintaining a private rebuild. Therefore these upstream
binary findings cannot be removed without either a refreshed official image
or an explicitly reviewed contract change. The successful synthetic
backup/check/restore test is recovery evidence, not a security waiver.

Re-run and replace this record for every release candidate. Do not carry the
counts forward after a base image, Go toolchain, Restic image, dependency, or
scanner database changes.
