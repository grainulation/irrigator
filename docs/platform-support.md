# Platform support

The Go runtime and terminal interface target macOS and Linux. On Windows, run the
Linux build inside **WSL 2**; see the [Windows (WSL) guide](windows-wsl.md).
**Native Windows is not currently supported; planned.** No release date is claimed.

| Component | macOS | Linux | Windows (WSL 2) |
| --- | --- | --- | --- |
| Runtime and CLI | Local execution, race-suite, and first-use verification; hosted macOS 15 ARM64 CI checks | ARM64: actual Linux execution, full race suite, static analysis, and first-use checks. AMD64: hosted Ubuntu 24.04 CI checks | Linux AMD64 build in Ubuntu 24.04 on a hosted Windows Server 2025 runner: first-use checks, race suite, integration suite without the race detector |
| Terminal dashboard and MCP server | `status --watch`, `graph`, `review tui`, `irrigator mcp` | Same commands | Same commands (Linux build) |
| Markdown skills | Requires a working CLI and the host's execution permission | Same contract; installing skills does not establish host-specific integration | Same contract, with the host running inside WSL |

The [platform workflow](../.github/workflows/platforms.yml) runs build, first-use,
race, static-analysis, and metadata checks on macOS and Linux, and builds
both architectures. Minimum Go is Go 1.26.7, matching `go.mod`. See
[workflow history](https://github.com/grainulation/irrigator/actions/workflows/platforms.yml)
for run results. The [Windows (WSL) workflow](../.github/workflows/windows-wsl.yml)
runs the same first-use and test targets inside WSL 2; its scope and limits are
listed in the [Windows (WSL) guide](windows-wsl.md#what-is-verified).

## What the verification establishes

Linux ARM64 checks ran as unprivileged processes on an actual Linux kernel in a
container. They establish execution on that environment, not a certification of
every distribution, filesystem, container configuration, or graphical browser.
The verified hosted run exercised build, first-use checks, the race suite,
and static analysis on Ubuntu 24.04 AMD64. Cross-building a binary alone does not verify process cleanup,
locking, filesystem confinement, or runtime behavior on the target.

The Linux scope is the CLI, its terminal dashboard and skills using the CLI.
Host authentication and real provider behavior need separate verification from mock orchestration tests.

## Toolchain requirements

Go 1.26.7 is the minimum supported version for every runtime feature; it is the
`go` directive in `go.mod`. The module pins the Go 1.27.2 toolchain used by
development and CI. Older toolchains must upgrade before building; there is no
reduced-feature fallback build with weaker or unavailable filesystem confinement.
The CI "Minimum Go" job builds with Go 1.26.7 and `GOTOOLCHAIN=local`.

The minimum covers workflow composition, launch contexts, confined
backup/campaign/recovery, and billing registry operations. To verify the minimum
with an installed Go 1.26.7 toolchain without automatic toolchain selection, set
`GOTOOLCHAIN=local` when running its `go test ./...` and `go build .`.

From the source root, `make build`, `make smoke`, `make demo`, and `make check`
provide the local runtime verification path. `make check` does not compile tests
behind the `integration` build tag (process supervision, controller drain and CLI re-exec tests);
`make check-integration` runs the whole tree with `-tags integration` under the race detector, and `make acceptance`
runs the full local acceptance set. The [Linux guide](linux.md) covers
first use. Keep run state on a filesystem with the locking, atomic replacement,
and permission semantics required by the runtime; unusual shared or network
storage is a separate configuration to verify.

## Distribution

A source checkout or local package does not imply an authenticated binary release
or an automatic update service. Consult the [CLI archive](runtime-release.md) and
[release](release-pipeline.md) instructions for the artifact you intend to use.

## Opt-in verification scripts

Two scripts check Irrigator against real tools. Neither runs from `go test`,
`make check` or CI.

**`tools/real-tools-smoke.sh [doctor] [tmux] [herdr] [container]`** builds the
binary into a temp directory, uses a temp run root, and prints `PASS`, `SKIP` (tool
not installed) or `FAIL` per check:

- `doctor`: `irrigator doctor` reports its executors section.
- `tmux`: runs `examples/terminal-swarm.json` with `--backend mock --executor tmux`
  on a private tmux server (a fresh `TMUX_TMPDIR`, `TMUX` unset; the default
  server is never touched), then checks that `irrigator cleanup --dry-run` keeps
  the run's session and `irrigator cleanup` removes it.
- `herdr`: the same mock run with `--executor herdr` against the real herdr-core
  (`hc`), then `irrigator cleanup`. This talks to your running herdr-core.
- `container`: a tiny Python worker in `python:3.13-alpine` through the first
  available of Apple `container`, podman or docker, which tries to open a TCP
  connection; the check passes only if the worker ran and the network was blocked.

**`tools/linux-test.sh [--no-race] [GO_TEST_ARGS...]`** runs `go vet ./...` and
`go test -race -count=1 ./...` on Linux in a container (Apple `container`,
podman or docker; override with `LINUX_TEST_RUNTIME`), as an unprivileged user,
with the repository mounted read-only and the module and build caches in a
temporary host directory. The image is `golang:<go.mod toolchain>`, falling back
to the toolchain's minor version and then the `go` line's minor version, with
`GOTOOLCHAIN=auto`. Real tmux is installed in the image and the tmux executor
tests are run again verbosely. Any `GO_TEST_ARGS` replace the default package list
(for example `./... -run Cleanup`). It needs network to pull the image
and download modules.

### What has and has not been verified

Verified with `tools/linux-test.sh` using Apple `container` on an Apple Silicon
host: the `golang:1.27.2` image (Debian, Linux arm64, Go 1.27.2) with tmux 3.5a
installed, running as an unprivileged user. `go vet ./...` and
`go test -race -count=1 ./...` passed, including the tmux executor and `irrigator
cleanup` tests. That run did not use podman or docker, and did not run on Linux
AMD64.

Not verified on Linux:

- The tmux executor tests drive a fake `tmux` script. Installing real tmux in the
  image does not change that, so a real tmux server on Linux is not exercised by
  the Go tests. `tools/real-tools-smoke.sh` exercises real tmux, but it has been
  run only on macOS.
- `tools/real-tools-smoke.sh` has not been run on Linux at all, so the container
  sandbox check on Linux (podman or docker) is unverified.
- The herdr executor and `irrigator cleanup` against a real herdr-core (`hc`) have
  not been verified on any platform by these scripts' author; the tests use a fake
  `hc`, and the `herdr` smoke check is left for the maintainer to run.
