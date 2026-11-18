# Versioned release checklist

The existing workflows build and test candidates; they do not publish a GitHub
Release. Use this checklist to promote reviewed CLI archives. The current
prerelease version is not evidence that a release has been published.

## Prepare one source revision

1. Check existing published versions and choose the next plain semantic version
   (`MAJOR.MINOR.PATCH`). Validate candidates privately; public releases use no
   `-rc.N` suffix. For an initial public release, consider `0.1.0`.
2. Update `internal/buildinfo/main.go`, `plugin.json`,
   `.claude-plugin/plugin.json`, and `.codex-plugin/plugin.json` together.
   Move the intended `CHANGELOG.md` entries into a dated version section.
3. Review and commit the release contents. Use a clean checkout for release
   validation. Do not delete unrelated local work just to make it clean.
4. Record the full commit SHA. Packaging with `--revision` uses committed Git
   bytes, so changes that have not been committed are excluded.

## Validate the source and the distributed artifacts

- Run `make acceptance` and `make lint` with the documented development
  toolchain. Retain the results for the release revision.
- Require successful macOS/Linux and Windows-through-WSL workflows for that
  same revision. Native Windows remains unsupported.
- Dispatch **CLI release candidates** at that revision. Require all four
  native targets: darwin/amd64, darwin/arm64, linux/amd64 and linux/arm64.
  Each job checks its archive through an isolated installation and mock run.
- Download each target into its own directory; their `SHA256SUMS` names
  otherwise collide. Verify the checksums and inspect `provenance.json` in
  each archive for the intended version, target and commit.
- If distributing the host plugins too, run the plugin packaging and consumer
  checks described in [local release packaging](local-release.md). CLI archives
  do not include the plugins.

The candidate workflow's fast checks supplement the full source gates; they do
not replace them. A local cross-build does not prove runtime behavior on another
platform.

## Promote the exact tested bytes

Create a version tag such as `vVERSION` at the reviewed commit, then create a
GitHub Release for that tag with the version's release notes. Attach the four
tested archives. Combine the four per-target checksum files into one
`SHA256SUMS`, or give each checksum asset a unique target-specific name;
duplicate asset names cannot coexist in one GitHub Release. Do not rebuild the
archives during promotion. Check the published asset
names, download links and installation instructions from a fresh consumer
environment.

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

A Homebrew formula, an installer and automated GitHub publishing can follow a
manual release. They are not prerequisites for publishing CLI archives.
