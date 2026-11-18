# Versioned release checklist

The existing workflows build and test candidates; they do not publish a GitHub
Release. Use this checklist to promote reviewed CLI archives. A source version
alone is not evidence that a release has been published.

## Prepare one source revision

1. Check existing published versions and choose the next plain semantic version
   (`MAJOR.MINOR.PATCH`). Validate candidates privately; public releases use no
   `-rc.N` suffix. The first public CLI release is `1.0.0`.
2. Update `internal/buildinfo/main.go`, `plugin.json`,
   `.claude-plugin/plugin.json`, `.codex-plugin/plugin.json`, and both version
   fields in `.claude-plugin/marketplace.json` together.
   Move the intended `CHANGELOG.md` entries into a dated version section.
3. Review and commit the release contents. Use a clean checkout for release
   validation. Do not delete unrelated local work just to make it clean.
4. Freeze the intended source and version before final validation. Record the
   full commit SHA. Packaging with `--revision` uses committed Git
   bytes, so changes that have not been committed are excluded.

## Validate the source and the distributed artifacts

- Run `make acceptance` and `make lint` with the documented development
  toolchain. Retain the results for the release revision.
- Require successful macOS/Linux and Windows-through-WSL workflows for that
  same revision, checking each workflow run's head SHA. A pull request may test
  a synthetic merge commit; rerun against the intended release commit when
  those differ. Native Windows remains unsupported.
- Dispatch **CLI release candidates** at that revision. Require all four
  native targets: darwin/amd64, darwin/arm64, linux/amd64 and linux/arm64.
  Each job checks its archive through an isolated installation and mock run.
- Download each target into its own directory; their `SHA256SUMS` names
  otherwise collide. Verify the checksums and inspect `provenance.json` in
  each archive for the intended version, target and commit.
- If distributing the host plugins too, run the plugin packaging and consumer
  checks described in [local release packaging](local-release.md). CLI archives
  do not include the plugins.

The candidate workflow runs only `make check-fast`; it supplements the full
source gates (`make acceptance` and `make lint`) and does not replace them. A local cross-build does not prove runtime behavior on another
platform. Any source change after the freeze requires a new candidate identity
and renewed affected checks. See [1.x compatibility](compatibility.md) for the
public contracts and explicit experimental exclusions.

## Promote the exact tested bytes

Create a version tag such as `vVERSION` at the reviewed commit, then create a
GitHub Release for that tag with the version's release notes. Attach the four
tested archives. Combine the four per-target checksum files into one
`SHA256SUMS`, or give each checksum asset a unique target-specific name;
duplicate asset names cannot coexist in one GitHub Release. Do not rebuild the
archives during promotion. Check the published asset
names, download links and installation instructions from a fresh consumer
environment.

Take all four archives from one **CLI release candidates** run dispatched at the
frozen commit; artifacts are named `irrigator-cli-<target>-<sha>` and are kept
for 30 days. Confirm each artifact's SHA and each archive's `provenance.json`
commit and version equal the release commit and version before attaching it.
Tag that exact commit: if the release branch is merged first, use a merge method
that preserves the SHA or rebuild and revalidate at the new SHA. Archives built
by GoReleaser or the dry-run workflow are a different build path and are not the
tested bytes. The `published: false` field in a candidate's `provenance.json`
describes the build and is not rewritten when the archive is published.

Publishing and tag creation are separate maintainer actions; no current workflow
performs them automatically. See [runtime archives](runtime-release.md) for
the current filenames and archive verification commands.

## Optional distribution work

Unsigned archives can be distributed with that status stated clearly. Checksums
detect corruption but do not authenticate a publisher. For Irrigator's signed
update format, establish key custody, trusted public-key distribution and a
monotonic sequence policy, then sign the exact tested assets using the
[release pipeline](release-pipeline.md). This is separate from Apple signing
and notarization.

The shared [Homebrew tap](homebrew.md) lives in `grainulation/homebrew`. Prepare
its pinned formula from the four frozen candidate archives. After publishing,
download all four release assets and `SHA256SUMS`, rerun the generator with
`--published-checksums`, and verify the output against the host-native archive.
Promote that formula in a separate distribution-only commit to the shared tap
and mirror it at `Formula/irrigator.rb` here for existing tap users. Do not move
the runtime release tag to that later formula commit. The HEAD recipe works from
`main` independently of the stable release. Homebrew, an installer and automated
publishing are not prerequisites for publishing CLI archives.
