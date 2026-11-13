# Release pipeline

Builds macOS and Linux CLI archives and signs them in Irrigator's own update
format. **Nothing is published.** The GitHub release is disabled, the release dry-run
workflow uploads nothing (the separate CLI release candidates workflow uploads
unsigned CI artifacts only; see [CLI runtime archives](runtime-release.md)), and
no signing key exists in this repository.

## Build archives

`.goreleaser.yaml` (GoReleaser v2) builds the module root for darwin and linux
on amd64 and arm64 with `CGO_ENABLED=0` and `-trimpath`, packs each as
`irrigator_<os>_<arch>.tar.gz` and writes `checksums.txt`. Native Windows is excluded
until the runtime is validated on real Windows hosts ([platform support](platform-support.md));
Windows users run the linux archive inside [WSL 2](windows-wsl.md).

```sh
goreleaser release --snapshot --clean --skip=publish
```

`dist/` is ignored by git. Snapshot builds are not signed and not versioned
releases.

## Sign one artifact

`tools/release-sign` is a stdlib-only program in the core module. It reads a raw
64-byte Ed25519 private key from a path you give it, and writes `release.json`
and `release.sig` for one artifact exactly as `irrigator update-check` expects
([update verification](update-verification.md)). It never generates a key, never
writes one and never touches the network.

```sh
mkdir -p ./signed/darwin_arm64
go run ./tools/release-sign \
  -key /path/outside/repo/release.key \
  -artifact dist/irrigator_darwin_arm64.tar.gz \
  -version 1.0.1 -os darwin -arch arm64 -sequence 5 \
  -out ./signed/darwin_arm64
```

Each artifact needs its own manifest because the manifest pins one OS and
architecture. `-out` must be an existing directory, so create it first. `-sequence` must be higher than any sequence already trusted by
the installing machine; `-valid-for` (default 30 days) sets expiry.

Verify with an independently trusted public key:

```sh
irrigator update-check --manifest signed/darwin_arm64/release.json \
  --signature signed/darwin_arm64/release.sig \
  --artifact dist/irrigator_darwin_arm64.tar.gz \
  --public-key trusted-release.pub --installed-sequence 4
```

Key custody, the sequence policy and any authenticated channel are decisions
outside this repository. Do not commit keys, `release.json` or `release.sig`.

## Tests

```sh
go test ./tools/release-sign
```

The test builds this repository's `irrigator`, signs a fixture with an
ephemeral in-memory key and checks it with `update-check`. It also confirms
rejection of a tampered artifact, a tampered manifest, a replayed sequence, an
unrelated key and an expired manifest.

## Dry run in CI

`.github/workflows/release-dry-run.yml` runs on manual dispatch and on pull
requests touching release files. It runs the signer test, then GoReleaser in
`--snapshot` mode with read-only permissions, and uploads nothing. The
`goreleaser-action` is pinned to a commit SHA, with the release tag in a trailing
comment, like the other actions in the workflows.

## Distribution

There is no Homebrew cask: casks are quarantined by macOS and would need
notarization, which this pipeline does not provide. The distribution paths are
a source build (`make build`), a source-built Homebrew formula that runs
`go build .`, or a curl installer that fetches an archive and runs
`irrigator update-check` before installing. No curl installer ships in this
repository yet; `make install` from a source checkout is the existing local path. A valid signature shows control of the
pinned key only; it is not Apple signing, notarization or an install step.
