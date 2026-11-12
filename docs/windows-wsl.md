# Windows: use WSL 2

Native Windows is not supported. On Windows, run Irrigator inside
[WSL 2](https://learn.microsoft.com/windows/wsl/) (Windows Subsystem for Linux):
it is the Linux build running on a real Linux kernel, with the same CLI,
terminal dashboard and skills described in the [Linux guide](linux.md).

The [Windows (WSL) workflow](../.github/workflows/windows-wsl.yml) checks this
path on every change. See [what is verified](#what-is-verified) for its exact scope.

## Set up

1. In PowerShell, install WSL with Ubuntu (one reboot):

   ```powershell
   wsl --install -d Ubuntu-24.04
   ```

   Confirm it runs as WSL 2 with `wsl -l -v` (the `VERSION` column must be `2`).
   WSL 1 is not verified.

2. Open the Ubuntu terminal and install the build tools. Ubuntu's own `golang`
   package is older than the module minimum, so install Go from go.dev:

   ```sh
   sudo apt-get update
   sudo apt-get install -y git make curl ca-certificates
   curl -fsSL https://go.dev/dl/go1.27.2.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
   echo 'export PATH="/usr/local/go/bin:$PATH"' >> ~/.profile && . ~/.profile
   go version
   ```

   Go 1.26.7 or newer works; development and CI pin Go 1.27.2. Use the
   `linux-arm64` archive on ARM Windows devices. Install `tmux` too if you want the
   [tmux executor](executors.md), and `gcc` if you run the race tests.

3. Clone and try the mock demo, from your Linux home directory:

   ```sh
   cd ~
   git clone https://github.com/grainulation/irrigator.git
   cd irrigator
   make build
   ./irrigator doctor
   ./irrigator demo --run ./runs/first-demo
   ```

   Continue with the [first-run guide](first-run.md).

## Keep work inside the Linux filesystem

Clone repositories and keep run directories under your WSL home (`~`), not under
`/mnt/c/...`. Windows drives mounted in WSL have different locking, permission
and symlink behavior from the Linux filesystem the runtime is verified on, and
Git checkouts there can pick up CRLF line endings. Run state on `/mnt` is an
unverified configuration (see [platform support](platform-support.md)).

From Windows, the WSL home is reachable in Explorer at `\\wsl$\Ubuntu-24.04\home\<user>`.

## Agent tools run inside WSL too

Irrigator starts provider CLIs (for example `claude` or `codex`) as Linux
processes, so install and sign in to them inside the Ubuntu terminal. Copies
installed on the Windows side are not used. The same applies to the
[Claude Code](claude-plugin.md) and [Codex](codex-plugin.md) plugins: run the host
inside WSL so it finds the Linux `irrigator` on `PATH`.

Windows Terminal supports the terminal features the dashboard uses (progress,
hyperlinks, clipboard); see [monitoring](monitoring.md).

## What is verified

The Windows (WSL) workflow runs on a hosted Windows Server 2025 runner with WSL 2
and Ubuntu 24.04 (x86_64), as an unprivileged user, with the checkout inside the
Linux filesystem:

- `make build`, `make smoke` and `make demo` (first use with mock workers);
- `make check`: the untagged test suite under the race detector, and `go vet`;
- the integration suite (`go test -tags integration ./...`: process supervision,
  controller drain, CLI re-exec) **without** the race detector. Under the race
  detector some integration tests overrun their fixed deadlines on this runner;
  race coverage of that suite comes from the native Linux and macOS jobs.

Not verified under WSL:

- WSL 1, distributions other than Ubuntu 24.04, and ARM64 Windows;
- checkouts or run state on `/mnt` (Windows drives);
- real provider CLIs, the [container sandbox](sandbox.md) (including Docker
  Desktop's WSL integration) and the herdr executor;
- a real tmux server: as on Linux, the tmux executor tests drive a fake `tmux`.

Native Windows (no WSL) remains unsupported and planned; see the
[roadmap](roadmap.md).
