<p align="center">
  <a href="https://irrigator.dev"><img src="docs/mascot.png" alt="Irrigator mascot" width="146"></a>
</p>

<h1 align="center">Irrigator</h1>

<p align="center"><strong>Durable local workflows for the coding agents you already use.</strong></p>

<p align="center">
  Plan the graph. Run in parallel. Inspect or resume from your terminal.<br>
  Explicit state, retries, and spending boundaries for local agent work.
</p>

<p align="center">
  <a href="https://irrigator.dev/"><img src="https://img.shields.io/badge/website-irrigator.dev-fcc830?style=for-the-badge" alt="See the Irrigator homepage"></a>
</p>

<p align="center">
  <a href="docs/platform-support.md"><img src="https://img.shields.io/badge/go-%E2%89%A51.26.7-00ADD8?logo=go&logoColor=white" alt="Go 1.26.7 or later"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT license"></a>
  <a href="https://deepwiki.com/grainulation/irrigator"><img src="https://img.shields.io/badge/docs-DeepWiki-1f6feb" alt="Docs on DeepWiki"></a>
</p>

<p align="center">
  <a href="https://github.com/grainulation/irrigator/actions/workflows/platforms.yml"><img src="https://github.com/grainulation/irrigator/actions/workflows/platforms.yml/badge.svg?branch=main" alt="macOS and Linux CI"></a>
  <a href="https://github.com/grainulation/irrigator/actions/workflows/windows-wsl.yml"><img src="https://github.com/grainulation/irrigator/actions/workflows/windows-wsl.yml/badge.svg?branch=main" alt="Windows (WSL) CI"></a>
</p>

<p align="center">
  <a href="#try-it-without-credentials">Quick start</a> ·
  <a href="docs/first-run.md">First run</a> ·
  <a href="#plugins">Plugins</a> ·
  <a href="docs/agent-setup.md">Agent setup</a> ·
  <a href="docs/index.md">Documentation</a> ·
  <a href="CHANGELOG.md">Changelog</a>
</p>

---


## What it adds

Irrigator runs coding agents in parallel while keeping results, retries and
spending evidence on disk. Use the CLI directly or through **Claude Code,
Codex and Antigravity plugins**. Inspect the work and resume from your terminal.

| Capability | What it gives you |
| --- | --- |
| **Durable execution** | Saved plans, attempts and accepted artifacts that survive interruptions. |
| **Coding cells** | One task per git worktree, with branches and objective checks to review. |
| **Bounded delegation** | Explicit task graphs, worker concurrency and provider limits. |
| **Recovery decisions** | Uncertain external work stays visible and is never silently replayed. |
| **Terminal review** | `status --watch`, `graph`, `review tui` and a read-only MCP view. |

## Try it without credentials

Requires **Go 1.26.7+**, `make` and a POSIX shell. Development and CI pin Go 1.27.2.
From a source checkout:

```sh
git clone https://github.com/grainulation/irrigator.git
cd irrigator
make build
./irrigator doctor
./irrigator demo
```

The demo uses mock workers, needs no credentials and makes no model calls.
Each invocation creates a fresh run directory and prints inspection commands.
For your own plan, follow the [first-run guide](docs/first-run.md).

To install from this checkout, run `make install PREFIX="$HOME/.local"` and add
`$HOME/.local/bin` to `PATH`. Installation copies the CLI; agent plugins and
provider tools have their own setup. CLI archives and their installation checks
are described in [runtime releases](docs/runtime-release.md).

Homebrew users can install the stable CLI from the shared [Grainulation tap](docs/homebrew.md):

```sh
brew tap grainulation/homebrew https://github.com/grainulation/homebrew
brew install grainulation/homebrew/irrigator
irrigator demo
```

The stable formula installs a pinned release archive and does not need Go.
The stable formula currently installs v1.0.0; the v1.1.0 GitHub release has no
binary assets yet. For a development source build, use
`brew install --HEAD grainulation/homebrew/irrigator`.
See the [Homebrew guide](docs/homebrew.md) for supported platforms, upgrades and checks.

### Ask your agent to start

Give Claude Code, Codex or Antigravity this prompt from the checkout:

```text
Read README.md and AGENTS.md in the Irrigator repository. Build ./irrigator with make build,
run ./irrigator agents and ./irrigator demo, and report the exit codes and final status lines.
Tell me which routes below this machine can run. Ask before any model-backed or paid run.
```

| Route | What it does | Needs | Cost and limits |
| --- | --- | --- | --- |
| **1. Mock demo and starter plan** | `demo`, `init`, `plan`, `apply --backend mock` | Local CLI | No model calls or API spend |
| **2. Local models** | Managed tasks using [Ollama](docs/managed-ollama.md) | CLI, Ollama and a local model | Local compute; no API spend |
| **3. Managed CLI profiles** | Turn/helper graphs via `claude:MODEL`, `codex:MODEL`, `agy:MODEL` | CLI and authenticated provider | Cost tracking by default; explicit enforced-budget profiles available |
| **4. Coding cells** | [`irrigator code`](docs/coding-cells.md), with branches to review | CLI, git repo and an agent CLI | Per-cell timeout; Claude USD cap; Codex/agy cost unknown, no USD cap |

Real providers use your account or local compute. New managed plans track
estimates without blocking when costs are unavailable; tracking thresholds warn.
Enforced budgets control admission and reservations and can still be overrun by
a provider. Unknown cost never means free execution. See
[cost tracking and enforced budgets](docs/execution-profiles.md#best-effort-cost-tracking).

## Put it to work

Start with a plan, validate it, then choose a backend explicitly:

```sh
./irrigator init --out plan.json
./irrigator plan plan.json
./irrigator apply plan.json --run runs/first --backend mock
./irrigator status runs/first
```

Use new plan filenames and run directories. For repository changes, coding
cells accept `--task ID=PROMPT` and `--verify ID=COMMAND`; results land on local
branches for review. A verification timeout or exit 77 leaves the result uncertain, as does a
successful check that changes the worktree or Git identity. See [coding cells](docs/coding-cells.md)
for setup, spending limits and recovery.

For graphs that grow during execution, choose
[recursive spawning](docs/swarm-shapes.md) or
[managed turns and helpers](docs/turn-shapes.md). These have separate execution
rules; worker concurrency is independent of graph size.

## Plugins

Install the `irrigator` runtime separately, then follow
[agent setup](docs/agent-setup.md) for your host:

| Host | Setup |
| --- | --- |
| Claude Code | [Claude Code plugin](docs/claude-plugin.md) |
| Codex | [Codex plugin or manual workflow](docs/codex-plugin.md) |
| Antigravity | [Antigravity plugin](docs/agent-setup.md#antigravity) |
| Other MCP hosts | [`irrigator mcp`](docs/mcp.md) |

The plugins share [Markdown skills](skills) and the local CLI under the host's
normal execution permissions. [Host integration](docs/host-integration.md)
covers installation and discovery checks; static package checks alone do not
establish that a host can load or invoke a plugin.

## Explore the docs

| Guide | Start here for… |
| --- | --- |
| [First run](docs/first-run.md) | Your own plan, provider selection and inspection. |
| [Coding cells](docs/coding-cells.md) | Parallel repository tasks and verification commands. |
| [Workflow diagnostics](docs/workflow-diagnostics.md) | Role routing, quota/reset advisories, delivery summaries and the offline benchmark. |
| [Attempt output](docs/attempt-output.md) | Paginated captured output with redaction and capture-loss metadata. |
| [Local review](docs/local-review.md) | Selective-file application, assembled-file checks and guarded undo. |
| [Workflow composition](docs/workflow-composition.md) | Reusable graphs and artifact contracts. |
| [Terminal workspace](docs/terminal-workspace.md) | Swarm dashboard and local review. |
| [Recovery](docs/recovery.md) | Interrupted calls, retries and execution authority. |
| [Architecture](docs/architecture.md) | Source layout, package layers and durable state. |
| [Compatibility](docs/compatibility.md) | The supported 1.x interfaces and saved-run formats. |
| [Documentation index](docs/index.md) | Accounting, providers, command references and more. |

## Verification and limits

The CLI supports **macOS and Linux**. Windows users run the Linux build inside
[WSL 2](docs/windows-wsl.md); native Windows is planned.
[Platform support](docs/platform-support.md) records the tested configurations.

Worktrees keep concurrent checkouts apart; they are not a host sandbox.
Approvals record a decision bound to a specific head and never merge changes.
See [SECURITY.md](SECURITY.md) for the local trust model.

CI runs builds, mock workflows, race tests and package checks. Native host and
live-provider acceptance are separate. Run the development gates with:

```sh
make acceptance
make lint
```

The [verification matrix](verification/README.md) details their scope, including
integration tests. [Contributing](CONTRIBUTING.md) covers setup and changes;
[choosing a tool](docs/alternatives.md) explains the tradeoffs.
See the [changelog](CHANGELOG.md) for releases and the
[release checklist](docs/versioned-release.md) for artifact promotion.

---

<p align="center">
  Built by <a href="https://grainulation.com">Grainulation</a> ·
  <a href="LICENSE">MIT licensed</a> ·
  <a href="https://github.com/grainulation/irrigator/issues">Issues & ideas</a>
</p>
