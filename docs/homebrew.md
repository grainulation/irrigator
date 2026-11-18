# Homebrew installation

The Irrigator repository also serves as its Homebrew tap. It provides the CLI
on macOS and Linux, including Linux inside WSL 2. Native Windows is unsupported.

## Stable installation

Install the pinned v1.0.0 release with:

```sh
brew tap grainulation/irrigator https://github.com/grainulation/irrigator
brew install grainulation/irrigator/irrigator
irrigator version
irrigator demo
```

The explicit repository URL is required: this tap lives in `irrigator`, rather
than a separate `homebrew-irrigator` repository. The formula is
[`Formula/irrigator.rb`](https://github.com/grainulation/irrigator/blob/main/Formula/irrigator.rb). It installs only the CLI;
agent plugins and provider tools still use their own [setup](agent-setup.md).
The stable formula chooses the verified release archive for the host's OS and
architecture, checks its SHA-256, and installs without a Go compiler.
If your Homebrew version requests formula trust, review the formula and run
`brew trust --formula grainulation/irrigator/irrigator`, then retry installation.

Use `brew upgrade grainulation/irrigator/irrigator` for subsequent stable
formula releases, and `brew test grainulation/irrigator/irrigator` to run the
credential-free installation check. Uninstall with `brew uninstall irrigator`.

## Development source installation

To build the current `main` branch instead, use the same tap with:

```sh
brew install --HEAD grainulation/irrigator/irrigator
```

`--HEAD` builds the current `main` branch and requires Homebrew's Go build
dependency (Go 1.26.7 or newer). It is a mutable development version, not a pinned
1.0.0 release. The build uses the installed compiler without automatic toolchain
downloads; an older compiler fails against `go.mod` rather than downloading the
development pin. For an existing development installation, use
`brew upgrade --fetch-HEAD grainulation/irrigator/irrigator`.

## Validate the recipe before release

```sh
make check-tools
make check-homebrew
```

`check-homebrew` requires Homebrew and a compatible Go compiler with the module
dependencies available. It registers an owned temporary local tap, loads the
actual formula through Homebrew, builds into a private prefix, and runs the
formula's credential-free demo/export test. It cleans up the temporary tap and
prefix. It does not install a keg, resolve/install formula dependencies, or
establish a fresh public-network `brew install`. Like `brew test`, it may
initialize Homebrew's own pinned formula-test gems on first use. CI runs it on
macOS; the Python candidate checks also run on Linux.

## Prepare a pinned release formula update

The checked-in stable stanza is generated from verified release archives, with
literal URLs and SHA-256 values. It has no placeholder checksums. For a release
update, wait for all four native candidate jobs to pass for one frozen runtime
commit. Download their archives into separate directories, verify their provided
checksums, and generate the new stable stanza (substitute the intended version):

```sh
python3 -I tools/homebrew_formula.py \
  --version 1.0.0 --commit FULL_FROZEN_RELEASE_SHA \
  --archive candidates/darwin-amd64/irrigator-1.0.0-darwin-amd64.tar.gz \
  --archive candidates/darwin-arm64/irrigator-1.0.0-darwin-arm64.tar.gz \
  --archive candidates/linux-amd64/irrigator-1.0.0-linux-amd64.tar.gz \
  --archive candidates/linux-arm64/irrigator-1.0.0-linux-arm64.tar.gz \
  --out /tmp/irrigator-release.rb
```

Use a new output filename. The generator refuses overwrites, missing or duplicate
targets, changed bytes, invalid archives, mismatched source identities, versions
or commits. It emits literal release URLs and archive SHA-256 values, preserving
the HEAD recipe. It does not publish assets or prove execution on another target;
the native candidate jobs establish that evidence.

Review and test that generated formula against the matching native archive:

```sh
python3 -I verification/check_homebrew.py \
  --formula /tmp/irrigator-release.rb \
  --archive candidates/darwin-arm64/irrigator-1.0.0-darwin-arm64.tar.gz
```

Choose the archive for the host running the check. The helper copies the formula
to a temporary tap under its required `Formula/irrigator.rb` filename, verifies
the loaded formula checksum against the candidate, installs its actual binary
recipe in a private prefix, and runs its exact-version/mock demo/export test.
It does not contact the release URL or install a keg globally.

Publish those same bytes following the [release checklist](versioned-release.md),
then copy the generated formula to `Formula/irrigator.rb` in a separate
distribution-only commit. Retain the release tag at the frozen runtime commit.
Committing formula hashes into that frozen commit would change archive
provenance and therefore change the hashes again. Do not rebuild or retag the
tested release to include its own hashes.

After that formula commit is available on `main` and its release assets are
published, the stable installation commands above select the updated release.
The formula does not install skills, provider credentials, background services,
or a hosted application.
