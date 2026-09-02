# Release bundle

The release is a pure Nix deployment bundle for one clean Git commit. Build it
on the build host and capture the single store path printed on standard output:

```sh
release_path="$(just release)"
```

Copy that directory into a previously absent, versioned directory on the
deployment host. The deployment host does not need Git, Nix, Just, Python, or
the project source tree:

```sh
rsync --archive --checksum -- \
  "$release_path/" <user>@<host>:<new-release-directory>/
```

Before use, verify every shipped file and load the three immutable production
images:

```sh
(cd <new-release-directory> && sha256sum --check SHA256SUMS)
docker image load --input <new-release-directory>/images/backup.tar.gz
docker image load --input <new-release-directory>/images/proxy.tar.gz
docker image load --input <new-release-directory>/images/vaultwarden.tar.gz
```

`release.json` identifies the commit, release, platform, archive paths, and
expected image references. It records build facts only; it is not a signature
or an attestation that optional checks were run. Keep private configuration and
all mutable state under a separate `DEPLOYMENT_ROOT`, never in the release
directory.

Continue with [Operations](docs/operations.md). Exact private-file formats are
in [Configuration](docs/configuration.md), provider setup is in
[Providers](docs/providers.md), and upgrades are in
[Upgrades](docs/upgrades.md).
