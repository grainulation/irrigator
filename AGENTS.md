# Working on Irrigator

Irrigator is a local Go execution engine, a terminal interface, a [MCP server](docs/mcp.md) and
agent skills. Start with README.md and docs/architecture.md.
The Go executable is main.go at the module root (an entry point only); the runtime is in internal/<domain>
packages with their unit tests beside them (layer table in docs/architecture.md). Build from the module root with make build or go build .

- Keep admitted plans, plan hashes, accepted results and historical attempts immutable.
- Preserve uncertainty about external effects. Never turn interrupted work into automatic replay.
- Maintain compatibility of versioned plans, worker protocol, checkpoint schemas and artifact names.
- Use mock providers for routine tests. Real provider calls require an explicitly authorized budget.
- Never commit credentials, local runs, generated binaries, dependency caches, research captures or device-specific paths.
- Follow docs/platform-support.md. Native Windows is not currently supported; planned. Windows users run the Linux build in WSL 2 (docs/windows-wsl.md), covered by the windows-wsl workflow. Do not infer runtime support from a cross-build alone.
- Use Go 1.26.7+ (the go.mod minimum; the toolchain is pinned to 1.27.2). Run make build, make smoke, make demo and appropriate tests. make check runs the default untagged race suite and vet. Integration tests (process supervision, controller drain, CLI re-exec) carry the integration build tag; make check-integration runs the whole tree with -tags integration. make lint and make check-exports are the static gates; make acceptance runs everything.
- For substantial work, split independent lanes with explicit ownership and integration responsibility. Keep verification independent from implementation where possible; do not fabricate agent activity or speedups.
- Preserve permission boundaries, budget reservations, process cleanup and local filesystem confinement in changes.
- State guarantees precisely: worktrees isolate concurrent checkouts and are not a host sandbox; an approval binds a workflow decision to a head and is neither a security boundary nor a merge; a budget controls admission and reservation, and a provider can overrun it.
- Keep commits focused and public-facing: explain the problem, resulting behavior and executed validation. Avoid private conversation or development-machine details.
- The interface is the terminal CLI on macOS and Linux.
