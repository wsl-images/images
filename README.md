# WSL Docker Images Builder

[![Build images](https://github.com/wsl-images/images/actions/workflows/build-wsl-images.yml/badge.svg)](https://github.com/wsl-images/images/actions/workflows/build-wsl-images.yml)
[![CI](https://github.com/wsl-images/images/actions/workflows/ci.yml/badge.svg)](https://github.com/wsl-images/images/actions/workflows/ci.yml)

Builds Linux/AMD64 container images from the `ModernDistributions` section of
[Microsoft's WSL distribution manifest](https://github.com/microsoft/WSL/blob/master/distributions/DistributionInfo.json).
Legacy Store/Appx distributions and ARM64 images are not built.

## Images and tags

Each upstream distribution gets its own lowercase repository:

```sh
docker pull ghcr.io/wsl-images/ubuntu:latest
docker pull ghcr.io/wsl-images/ubuntu-24.04:24.04
docker run --rm -it ghcr.io/wsl-images/debian:latest /bin/sh
```

Tags include the root filesystem's `VERSION_ID` (or `BUILD_ID`, then `rolling`
for unversioned rolling releases), a UTC build timestamp, `sha256-<upstream SHA256>`,
and `latest`. A SHA tag identifies the downloaded archive; it is not an OCI image
digest. Pin the registry digest (`image@sha256:...`) when an immutable container
image is required. `Ubuntu` follows Microsoft's default Ubuntu release;
`Ubuntu-24.04` and similar entries track their named releases.

When enabled, the Quay mirror uses the same tags prefixed with the distribution:

```sh
docker pull quay.io/wsl-images/images:ubuntu-latest
docker pull quay.io/wsl-images/images:ubuntu-24.04-24.04
```

These are imported WSL root filesystems with `/bin/sh` as the default command.
They are not upstream vendors' official container images, and running a container
does not test WSL registration, systemd startup, or the original WSL first-run flow.

## Automated builds

The daily workflow runs at **03:23 UTC** and can also be triggered manually.
It validates and snapshots the current manifest into a sorted build matrix.
Each distribution runs independently, with up to four jobs at once. One failed
download, import, or mirror push does not cancel the other distributions.

For each image the builder:

1. Downloads with HTTP status checks, bounded timeouts, and retries.
2. Verifies Microsoft's SHA256, including upstream checksums prefixed with `0x`.
3. Detects gzip, bzip2, xz, zstd, or plain tar regardless of the `.wsl` suffix.
4. Reads `os-release` from the tar without extracting files onto the host.
5. Imports with an explicit AMD64 platform, default shell, and provenance labels.
6. Runs a container with networking disabled to verify its shell and `os-release`.
7. Pushes only this build's tags to GHCR, then to the optional Quay mirror.
8. Saves a success marker only after all enabled destinations succeed.

Success cache keys include the distribution, upstream checksum, builder/workflow
contents, and mirror configuration. Failed builds are retried on the next run.
Cache eviction can cause a safe rebuild. No downloaded root filesystems are cached.
Updating source code does not automatically publish images: CI runs on pushes and
pull requests, while publishing uses the schedule or a manual dispatch.

### Run or repair a build

Use **Actions → Build and Push WSL Docker Images → Run workflow**:

- `distributions`: optional comma-separated names such as `Ubuntu-24.04,Debian,archlinux`.
- `force`: rebuild even if this source and builder have a success marker.
- `publish`: turn off to download, verify, import, and smoke-test without pushing.

The **Manual Rebuild** workflow calls the same implementation with `force=true`.

```sh
# List current names without downloading root filesystems.
go run . --matrix

# Force a targeted published rebuild.
gh workflow run build-wsl-images.yml -R wsl-images/images \
  -f distributions=Ubuntu-24.04 -f force=true -f publish=true

# Build all changed/missing images.
gh workflow run build-wsl-images.yml -R wsl-images/images
```

GitHub can [disable schedules after 60 days without repository activity](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule)
in public repositories. Successful scheduled runs alone do not prevent this.
If daily runs stop, check the workflow's state and re-enable it:

```sh
gh api repos/wsl-images/images/actions/workflows/build-wsl-images.yml --jq .state
gh workflow enable build-wsl-images.yml -R wsl-images/images
gh workflow run build-wsl-images.yml -R wsl-images/images
```

An HTTP error or checksum mismatch is reported against its individual distribution.
Do not bypass checksum verification: check the vendor download and Microsoft's
manifest for a corrected release. If a registry image was removed manually, use
`force=true` to repair it even if its success marker is still cached.

## Registry configuration

GHCR uses the workflow's `GITHUB_TOKEN` with `packages: write`. Existing packages
must grant this repository Actions write access. Newly created packages may need
their visibility changed to public in GitHub's package settings before anonymous
users can pull them. The builder adds the repository source label for linkage.

Quay is enabled only when `QUAY_ROBOT_TOKEN` is present and the repository variable
`QUAY_ENABLED` is not `false`. Optional variables:

| Variable | Default |
| --- | --- |
| `QUAY_USERNAME` | `wsl-images+builder` |
| `QUAY_REPOSITORY` | `quay.io/wsl-images/images` |
| `QUAY_ENABLED` | Enabled when the token exists |

A Quay login failure still allows GHCR publication, but the job reports failure
and does not save a success marker. Repair the token or explicitly disable the
mirror. Quay push failures also remain visible after GHCR has published.
Forks should configure their own Quay namespace before adding a token.

## Local development

Use Go **1.26+**, a Linux Docker engine, `xz`, and `zstd`. Linux or WSL is recommended.
Go tests can also run on Windows; unavailable external compression tools are skipped
there and exercised on Ubuntu CI. The application has no third-party Go dependencies.

```sh
git clone https://github.com/wsl-images/images.git
cd images
go test -race -cover ./...
go vet ./...
go build ./...
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

# Builds one image locally; publishing is opt-in.
go run . --distro Ubuntu-24.04

# After docker login ghcr.io:
go run . --distro Ubuntu-24.04 --owner YOUR_NAMESPACE --push
```

Use `go run .`, not `go run main.go`, because the builder spans multiple files.
Temporary downloads and decompressed archives are removed on completion or error.
Imported local Docker images remain available under `wsl-local/<name>:sha256-<hash>`.
Allow disk space for both the compressed download and decompressed root filesystem.
