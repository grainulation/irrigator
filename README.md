<p align="center">
  <img src="docs/mascot.png" width="144" alt="Irrigator mascot">
</p>

<h1 align="center">Irrigator</h1>
<p align="center"><strong>Durable local workflows for agent work.</strong></p>
<p align="center">
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-blue"></a>
  <a href="docs/platform-support.md"><img alt="Runtime: macOS, Linux and Windows via WSL 2" src="https://img.shields.io/badge/runtime-macOS%20%7C%20Linux%20%7C%20WSL%202-476b46"></a>
</p>
<p align="center">
  <a href="https://github.com/grainulation/irrigator/actions/workflows/platforms.yml"><img alt="macOS and Linux CI" src="https://github.com/grainulation/irrigator/actions/workflows/platforms.yml/badge.svg"></a>
  <a href="https://github.com/grainulation/irrigator/actions/workflows/windows-wsl.yml"><img alt="Windows (WSL) CI" src="https://github.com/grainulation/irrigator/actions/workflows/windows-wsl.yml/badge.svg"></a>
</p>

Irrigator is for developers who want coding agents to work in parallel without
losing track of results, retries, or spending. Turn a task into an explicit graph,
run it locally, and inspect or resume the work from your terminal.

Use the **CLI** directly or through the **Claude Code, Codex and Antigravity plugins**. The
interface is the terminal: `status --watch`, `graph`, `review tui`, `session`,
and a read-only [MCP server](docs/mcp.md) for agent hosts.

[First run](docs/first-run.md) · [Plugins](#plugins) · [Documentation](docs/index.md) · [Architecture](docs/architecture.md) ·
[Contributing](CONTRIBUTING.md)

## Try it without credentials

You need Go 1.26.7 or newer (the module's `go` directive), `make`, and a POSIX
shell. Development and CI checks pin the Go 1.27.2 toolchain. The executable
source is `main.go` (the entry point) and `internal/` (the packages; see [Architecture](#architecture)).
From the repository root:

```sh
make build
./irrigator doctor
./irrigator demo --run ./runs/first-demo
```

The demo runs a small swarm of mock workers, with a live terminal map of task
states. It needs no credentials and makes no model calls. Use a fresh run directory
for each demo. See the [first-run guide](docs/first-run.md) for your own plan,
provider selection, and recovery.

## Why Irrigator?

A shell script or Makefile is often enough for a fixed sequence of commands.
Irrigator adds durable task identities, accepted artifacts, bounded agent
expansion, and an explicit recovery decision when a provider's outcome is unknown.
It is designed for local agent workflows, rather than a hosted workflow service.
See [choosing a tool](docs/alternatives.md) for the tradeoffs.

```sh
make smoke   # First-use workflow and artifact export
make demo    # Failed acceptance, revision, and recovery with mock workers
make check   # Race tests and static analysis
make check-integration  # Tagged supervisor/drain tests (not part of make check)
make replay-gate        # Experimental: redacted failure fixtures replay through the real executor path
make acceptance         # check + check-integration + tool checks + replay-gate + smoke + demo + durability
```

To install the CLI explicitly into your user prefix:

```sh
make install PREFIX="$HOME/.local"
```

Alternatively, `go install .` installs from this checkout into
`GOBIN` (or the default Go bin directory).

Add `$HOME/.local/bin` to your `PATH` when using `make install`. Building or running the demo does not install
skills, configure an agent host, download models, or start a background service.

## What it provides

- **Durable execution.** Immutable admitted plans, recorded attempts, accepted
  artifacts, and transactional revisions of unfinished work.
- **Explicit contracts.** Required outputs and configured acceptance checks gate
  downstream work. Reusable workflows support bounded typed JSON artifacts.
- **Bounded delegation.** Separate main-agent identities, optional sequential
  turns, and terminal helpers. Worker concurrency is an independent setting.
  Dependencies pass result data between tasks; git preparation, acceptance and
  settlement are explicit steps. Per-task git worktrees keep concurrent
  checkouts apart. They are not a host security sandbox.
- **Provider accountability.** Pinned execution profiles, operation reservations,
  and receipts. A budget is an admission and reservation control, so a provider
  can still overrun it. Unknown charges and uncertain effects remain visible.
- **Approvals.** An approval records a workflow decision bound to a specific
  head. It is not a security boundary and never merges anything.
- **Recovery with provenance.** Back up run evidence or transfer execution
  authority through exclusive recovery. An uncertain call is not silently replayed.
- **Observation and review.** `status --watch` and `graph` display execution
  state. `review tui` supports interactive review, including recording head-bound
  approvals and change requests; it does not merge changes or start graph work.

Real providers require their own installed tools or server, configuration, and
credentials. Start with [execution profiles](docs/execution-profiles.md) and
[local Ollama](docs/managed-ollama.md). Authorize spending explicitly: reservation
limits control admission and do not guarantee the final provider invoice.

## Two ways to run work

| You want | Use |
| --- | --- |
| Agents that can split their task and hand the pieces to sub-agents, which can split again | **Spawning trees**, with mock workers, the built-in Haiku backend or your own worker command. You choose how many tasks to start with, how many levels deep it may go and how many children each task may create. See [recursive spawning](docs/swarm-shapes.md). |
| A chosen model or provider for each agent, and a budget for the whole run | **Managed profiles**: a fixed number of main agents, each taking up to a set number of turns, optionally with helpers. Helpers cannot spawn further work. See [turn shapes](docs/turn-shapes.md) and [execution profiles](docs/execution-profiles.md). |

The two are separate ways of growing a run, not two spellings of the same thing.
The detailed docs write these limits as short shapes (`A×D×S` for trees, `X×Y×Z`
for turns); you do not need them to get started. How many workers run at once is
a separate setting in both.

## Choose your interface

| Interface | Start here |
| --- | --- |
| CLI and terminal dashboard | [Linux guide](docs/linux.md), [workflow composition](docs/workflow-composition.md) |
| Agent skills | [Host integration](docs/host-integration.md), [skills](skills) |
| Agent plugins (Claude Code, Codex, Antigravity) | [Agent setup](docs/agent-setup.md) |
| Other MCP hosts (Cursor, OpenCode and others) | [MCP server](docs/mcp.md) |
| Terminal workspace with Swarm and Review | [Workspace guide](docs/terminal-workspace.md), [local code review](docs/local-review.md) |
| Recovery and accounting | [Recovery](docs/recovery.md), [billing reconciliation](docs/billing-reconciliation.md) |

Skills use a separately installed `irrigator` executable under the host's normal
execution permissions.

## Plugins

Claude Code, Codex and Antigravity get a full plugin from this repository; other
MCP agents (Cursor, opencode and others) connect to `irrigator mcp`. Start with
[agent setup](docs/agent-setup.md).

| Host | Package metadata | Setup |
| --- | --- | --- |
| Claude Code | [.claude-plugin/](.claude-plugin) | [Claude Code installation](docs/claude-plugin.md) |
| Codex | [.codex-plugin/](.codex-plugin) and [plugin.json](plugin.json) | [Codex setup](docs/codex-plugin.md) |
| Antigravity | [plugin.json](plugin.json), [mcp_config.json](mcp_config.json) and [mcp.json](mcp.json) | `agy plugin install https://github.com/grainulation/irrigator` ([agent setup](docs/agent-setup.md#antigravity)) |

These are thin host-specific metadata layers around the same [Markdown skills](skills)
and local CLI. Install the runtime separately, then follow the setup guide for
your host. See [host integration](docs/host-integration.md) for installation and discovery
checks in your host. Antigravity replaced Gemini CLI; there is no separate Gemini plugin.

## Platform status

The CLI and terminal dashboard support **macOS and Linux**. On **Windows, use
[WSL 2](docs/windows-wsl.md)**: the Linux build inside Ubuntu, checked in CI.
Native Windows is not supported yet (planned).

[macOS and Linux CI](https://github.com/grainulation/irrigator/actions/workflows/platforms.yml)
runs builds, mock demos, race tests, package checks, and cross-builds.
[Windows (WSL) CI](https://github.com/grainulation/irrigator/actions/workflows/windows-wsl.yml)
runs the build, mock demos and test suites inside WSL 2 on a Windows runner. See
[platform support](docs/platform-support.md) for tested configurations.

This is a pre-release source distribution. See [CHANGELOG](CHANGELOG.md) for
compatibility changes and [SECURITY](SECURITY.md) for the local trust model.
Maintainers can prepare [CLI archives](docs/runtime-release.md); that step does
not publish a release.

## Architecture

Irrigator is one local executable. Durable run state on disk decides what runs next; terminal
commands, the [MCP server](docs/mcp.md) and host skills read or drive that state and never keep a second
copy. The long form, with a package-by-package table, is [docs/architecture.md](docs/architecture.md).

### Entry points to packages

| Entry point | Goes through | Responsibility |
| --- | --- | --- |
| `main.go` | `internal/cli` | `os.Exit(cli.Main(os.Args))`: dispatch, help table, built-in commands |
| terminal commands (`apply`, `resume`, `status`, `cancel`, `retry`, `deliver`, ...) | `runops`, `delivery`, `dashboard`, `graph`, `checkpoint` | controller loop, cancel/retry/resolve, inspect, journaled delivery, status UI |
| `irrigator mcp` | `mcpserver` | MCP view of run state; read-only unless started with `--allow-control` |
| worker process (`_worker`, `_supervise`) | `executor`, `launch`, `managed`, `provider` | one task attempt, process supervision, provider adapters |

### Layers and dependency arrows

Each package lives in a numbered layer and may import only lower layers. The compiler rejects cycles,
and every package's `doc.go` states its layer and its actual imports. Arrows point from a layer to what
it builds on:

    main -> cli (32)
      -> mcpserver (31) -> delivery (30) -> runops (29) -> dashboard (28)
      -> selfupdate, gitcontract, branchreview, debug, telemetry (27..23)
      -> session, approvals, executor, provider (22..19)
      -> backup, sizing, observer, composition, campaign, messaging, graph, managed (18..11)
      -> checkpoint (10) -> artifactstore (9) -> launch (8) -> model (7)
      -> leaves (6..0): commandflags, terminalview, localfs, jsonio, buildinfo, cost, clock

`textsafe`, `providerenv`, `store`, `review` and `reviewtui` predate the split and sit outside the numbering.

### Persistence invariants

Each line names a test that pins it (test names are stable; the files sit beside the owning package).

- State changes go through `checkpoint` transactions: the callback runs once on a private snapshot, only a
  successful callback is published, and a failed publication is not retried (`TestCheckpointStorePublicationFailureDoesNotRetry`,
  `TestCheckpointStoreConformance`).
- Artifacts are immutable and verified on read; resolution cannot escape the run directory
  (`TestStoreRoundtripImmutable`, `TestStoreRejectsPartialOrIncorrectContent`, `TestLocalNestedKeysAndParentEscape`).
- A copied or restored run has no authority until recovery admits it (`TestRecoveryNestedAliasCannotAcquireLease`,
  `TestRecoveryTargetReplacementAndArchiveTampering`).
- Process supervision never trusts a persisted PID, and an unconfirmed cleanup blocks replay
  (`TestWorkerDeathRetainsLeaseUntilProviderStops`, `TestUnconfirmedSupervisorCleanupBlocksSafeReplay`).
- Uncertain external work stays uncertain; it is never replayed automatically (`TestSupervisorDeathMakesReplaySafeNodeUncertain`).
- An approval link yields at most one decision (`TestApprovalsConcurrentPostsDecideOnce`).
- Plans are strict: typos and trailing data are rejected (`TestPlanRejectsTyposAndTrailingData`).

### Tests

    make check-fast          # fast feedback: every internal package's unit tests + vet
    make check               # untagged race suite + vet
    make check-integration   # whole tree with -tags integration under the race detector
    make check-tools         # Python tool and repository-consistency tests
    make lint                # golangci-lint, gofmt, go mod tidy -diff
    make check-exports       # exported-surface report (internal packages)
    make acceptance          # everything above plus replay gate, smoke, demo, durability

Plain `go test ./...` does not run the integration tests (they carry the `integration` build tag); use
`go test -tags integration ./...` or `make check-integration`.

## Contribute

Start with [CONTRIBUTING.md](CONTRIBUTING.md) for development checks, architecture
invariants, and focused pull requests. Report vulnerabilities through the process
in [SECURITY.md](SECURITY.md).

Irrigator is licensed under [MIT](LICENSE). Third-party components retain their
own licenses; see the [license inventory](docs/license-inventory.md).

### Cost and usage

New managed plans track cost estimates without blocking work when a provider
cannot report them. USD estimates, token usage, and externally verified charges
are separate; unavailable cost never means free execution. Tracking thresholds
warn rather than stop a run. Existing enforced-budget plans retain their limits,
and uncertain execution still requires safe recovery. See
[cost tracking and enforced budgets](docs/execution-profiles.md#best-effort-cost-tracking).
