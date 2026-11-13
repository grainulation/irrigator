# CLI runtime archives

`tools/package_runtime.py` builds local, unsigned CLI archives for macOS and Linux,
with amd64 and arm64 binaries. It does not publish, install, or establish that a
cross-built binary has been executed on its target.
Native Windows is not currently supported; planned. On Windows, use the Linux
archive inside [WSL 2](windows-wsl.md).

Use an installed Go toolchain compatible with `go.mod`. The packager disables
network module access and automatic toolchain downloads; dependencies must already
be available in the local module cache. It builds with CGO disabled, `-trimpath`,
`-buildvcs=false`, and an empty build ID. Credentials and arbitrary caller Go flags
are not passed to the compiler.

```sh
python3 -I tools/package_runtime.py --go /path/to/go --out ./irrigator-runtime-release
python3 -I tools/test_package_runtime.py
```

The default refuses tracked changes and untracked files. To intentionally build a
committed snapshot while other work is present, pass `--revision COMMIT`. This
selects Git object bytes, **not** the working tree. Review the resolved commit in
`provenance.json`; uncommitted fixes will not be included. Source inputs are limited
to `main.go`, `internal/`, `examples/`, module files, README, LICENSE, and the
version manifest. Symlinks and submodules in those inputs are rejected. The version
comes from that snapshot's `plugin.json`; packaging does not bump manifests.

By default all four targets are built. Repeat `--target darwin/arm64` or
`--target linux/arm64` to select targets. The output directory must not exist and
its parent must exist; existing files and symlinked output paths are refused.
Compilation finishes before output creation. If writing output fails, a partial
new directory may remain: inspect it and choose a fresh destination for retry.

Each `irrigator-VERSION-OS-ARCH.tar.gz` contains a single directory with:

- `irrigator`, executable without a Go installation;
- `LICENSE`, the source README, and standalone `INSTALL.txt` instructions;
- `provenance.json`, recording the exact Git commit, selected source file digests,
  aggregate source digest, toolchain identity, target and executable digest;
- `SHA256SUMS`, covering all other files in that directory.

The output directory also contains checksums for every archive. Archives have
sorted members, fixed timestamps, fixed permissions and no machine-specific owner
names. With the same source, toolchain, target and dependencies, repeated builds
should produce identical archives. Checksums detect changes; they do not establish
publisher authenticity. No signing or notarization is performed. Provenance
includes source-relative paths only. Source review remains necessary: this tool
cannot prove that source code or embedded resources contain no sensitive data.

For a matching platform, inspect and verify before installation:

```sh
shasum -a 256 -c SHA256SUMS
tar -tzf irrigator-VERSION-OS-ARCH.tar.gz
tar -xzf irrigator-VERSION-OS-ARCH.tar.gz
cd irrigator-VERSION-OS-ARCH
shasum -a 256 -c SHA256SUMS
./irrigator help
./irrigator demo --help
install -m 0755 irrigator "$HOME/.local/bin/irrigator"
```

Create `$HOME/.local/bin` first if needed and add it to your PATH. The included
README describes the full repository and contains relative links to files absent
from a binary archive; use the [source repository](https://github.com/grainulation/irrigator)
for those pages and the [first run guide](first-run.md) for a mock workflow. Skills
and host plugin metadata are distributed separately; see [local plugin packaging](local-release.md).

## Packaged artifact acceptance

`python3 -I tools/check_runtime_archive.py ARCHIVE.tar.gz` validates the exact
member set, safe regular-file paths and modes, size bounds, checksums and binary
provenance. It refuses a target that differs from the host, then copies the CLI
into a temporary user prefix and runs `version`, `help`, a mock demo, `status`, and
`export` with a private HOME and no inherited provider credentials. Temporary
files are removed afterward. Run it only on an archive whose source you trust:
checksums do not make executable code trustworthy.

The macOS/Linux workflow builds an archive from immutable `HEAD` for each runner's
native Go target and invokes this checker. This tests the packaged artifact in
addition to the source suite; adding the workflow step does not itself establish
that a hosted run has passed. `tools/test_check_runtime_archive.py` exercises
unsafe member types and paths, duplicates, unexpected files and checksum failures.

## Downloadable candidates

The manually triggered **CLI release candidates** workflow builds four archives
on native runners: macOS amd64/arm64 and Linux amd64/arm64. Each job verifies its
actual architecture and installs/tests its own archive in an isolated temporary
home before uploading it with checksums. Select the intended source revision
when dispatching; the artifact name and internal provenance identify that commit.
Download the candidate from the workflow run's artifacts section. Access follows
repository permissions and artifacts expire after 30 days.

These are unsigned CLI candidates. No Apple account is required to build them. The workflow uses read-only repository
permissions and does not create tags, publish a GitHub Release, change visibility,
or update the homepage. A maintainer can promote the exact reviewed archives and
checksums into a versioned release after the full platform checks pass. Retain
per-target checksums when downloading multiple candidate artifacts; their
`SHA256SUMS` filenames otherwise collide in a shared extraction directory.

Homebrew formula/tap publication is a separate distribution step.
