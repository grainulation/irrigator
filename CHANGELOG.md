# Changelog

Changes below describe source revisions. A source commit is not a signed binary
release. Versioned plan and worker protocols are tracked separately from the CLI.

## Unreleased

### Added

- Antigravity plugin: `agy plugin install https://github.com/grainulation/irrigator` installs the skills and the
  read-only `irrigator mcp` server (root `plugin.json` with the Agent Plugins schema, `mcp_config.json`, `mcp.json`).
  New [agent setup](docs/agent-setup.md) guide for Claude Code, Codex, Antigravity and other MCP agents.
- Windows via WSL 2: a [guide](docs/windows-wsl.md) and a `windows-wsl` CI workflow that runs the Linux
  build, mock demos and test suites inside Ubuntu 24.04 on a Windows runner. Native Windows remains unsupported.
- Experimental: `irrigator debug fixture export RUN_DIR --out FILE` writes a redacted replay
  fixture (`irrigator.replay-fixture/v1`) of a run, and `make replay-gate` replays the committed
  dev and held-out fixtures through the real executor path. See [docs/debug-workflow.md](docs/debug-workflow.md).

### Changed

- The minimum Go version is now 1.26.7 (the `go.mod` directive), required by `charm.land/lipgloss/v2` v2.1.0.
- Source layout: the entry point is now only `main.go` at the module root; the runtime lives in `internal/<domain>`
  packages with unit tests beside them (see [docs/architecture.md](docs/architecture.md)). No change to
  CLI output, JSON schemas or the worker protocol.
- Plain `go test ./...` no longer runs the integration tests: they carry the `integration` build tag.
  Use `go test -tags integration ./...` or `make check-integration` (now unfiltered, whole tree, race
  detector). New gates: `make lint` (golangci-lint) and `make check-exports`.
- Newly scaffolded managed plans use best-effort cost tracking. Unknown cost
  does not block completed work or reserve funds in tracking mode; existing
  enforced plans retain their budget semantics and execution-safety checks.
- Cost telemetry labels USD estimates explicitly, separately from usage and
  externally reconciled charges, and warns when a tracking threshold is exceeded.

- Terminal-first consolidation: the CLI, terminal review/session views and the
  [MCP server](docs/mcp.md) are the supported surfaces.
- **Breaking:** the app-only `panel`, `show` and `host` commands are removed rather than stubbed, and now fail as unknown commands. The legacy installer,
  retained native license notices, unused debug/recording projections,
  and obsolete handoff docs are removed.
- Correctness: `gc` only removes obsolete attempt logs inside a valid, quiescent
  run; it does not reclaim Git worktrees, delete branches, or infer worker
  liveness from a stored controller PID. ACP worktrees, branches and dirty
  content remain for explicit recovery.
- `make check` is the race and vet suite over default (untagged) tests;
  `make check-integration` runs the tagged supervisor/drain tests and
  `make acceptance` aggregates both with tool checks, smoke, demo and the
  swarm durability check.

- Legacy `apply`, `prepare`, and `deliver` require explicit backend selection.
  Managed plans continue to use their declared profiles; resume keeps the
  admitted backend. There is no implicit paid provider.
- `--call-budget` names the per-call limit; `--budget` remains a compatible alias.
- Go 1.26 is the minimum supported toolchain (the `go.mod` directive). Older
  reduced-capability fallback builds have been removed; development and CI pin
  Go 1.27.2.
- Terminal rendering and provider environment policy have separate internal modules.

### Added

- Explicit reviewed provider executable migration for paused managed runs,
  preserving original plans, historical bindings, results, and budget receipts.
- Worker boundary snapshots and model schemas for remaining depth, turns,
  helpers, and task capacity; known invalid final-turn waits fail admission.
- `make check-fast` for contributor feedback alongside the unchanged full race
  gate, and a manual four-platform CLI candidate packaging/install workflow.

- Offline managed-plan scaffolding with executable pinning, explicit context-file
  imports, and optional per-profile deadlines. Resume preserves saved call limits.

- Reproducible macOS/Linux CLI archives with source provenance, checksums and
  isolated installation acceptance in the platform workflow.
- Internal checkpoint transaction, attempt executor and immutable artifact-store
  boundaries, wired to local execution and exports, with alternate-adapter
  conformance tests and a staged hosting architecture plan.
- Stable terminal swarm slots, distinct state markers and static/no-color modes.
- Machine-readable command help derived from actual flag definitions.
- A generated plan JSON Schema for editor assistance.
- A first-run tutorial, tool comparison and contributor conduct guidelines.

### Fixed

- Preserve admitted child graphs and historical spending when retrying failed
  parents; report retry no-ops and backup execution-authority boundaries.

- Reject unreconciled running paid work at the cell-admission boundary, including
  direct internal-worker invocation, without changing safe mock recovery.

- Accept explicit empty turn requests as lane completion while keeping ordinary
  required outputs nonempty; report deterministic nested turn-contract errors.
- Explain structured-output retry exhaustion, invalid context keys, and session
  registry failures with actionable diagnostics.
- Remove incidental subprocess startup deadlines from conversation scope unit
  tests while retaining process-backed history coverage.

- Confirm provider cleanup before replaying uncertain work; explain automatic
  retry exclusions without treating paid providers as replay-safe.
- Preserve billed provider failures, distinguish unknown cost from explicit zero,
  and keep timeout and authentication diagnostics actionable.
- Constrain generated child manifests on the model wire while retaining their
  canonical checkpoint representation and failed-attempt evidence.
- Stop stale status watches after controller loss and accept verified macOS
  system temporary-directory aliases in backups.

- Crash recovery retains valid reported charges even when provider cleanup is
  unconfirmed, while preserving billing uncertainty and replay restrictions.
- Controller errors drain owned workers before returning; unresolved cleanup
  remains explicit rather than authorizing replay.
- Provider receipt publication failures retain uncertain charges and repair
  accounting before unrelated work continues within the remaining allowance.
  Reused sessions cannot exceed the next call's reservation.
- Task names cannot alias active or retired cells on case-insensitive filesystems.
  Artifact exports reject colliding paths and never overwrite an existing file.
- Reinstall checks cached contents against the supplied trusted archive;
  packaging rejects linked output ancestors before creating files.
- Interrupted built-in paid calls no longer replay automatically. Reported costs
  are retained and ambiguous legacy charges remain visible across explicit retry.
- Cancelling a node during acceptance no longer aborts sibling work.
- Claude subprocesses receive a restricted environment instead of unrelated
  credentials and runtime control tokens.
- Plan parse errors identify their source and syntax position; cycle errors list
  unresolved task IDs.

## 1.0.0-rc.4 — Initial source baseline

- Durable local task execution, explicit provider profiles, artifact acceptance,
  recovery, read-only observation, and Claude Code/Codex workflow skills.
- Optional macOS terminal sources and pinned native dependencies.
- MIT license and macOS/Linux verification workflows.
