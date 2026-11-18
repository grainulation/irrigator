# Local reliability and observability draft

Status: working draft on `draft/docs-alignment-20261117`. No remote publication is part of this draft.

## Implemented scope

This draft aligns documentation across Homebrew, plugin MCP registration, and package architecture, and integrates coder reliability diagnostics and observability enhancements into the Go implementation:

- **Strict non-additive token accounting (`internal/coder`)**: Terminal usage rejects negative, non-integer, boolean, overflowing, or alias-conflicting token fields; `input_tokens` is never inflated with `cache_read_tokens`, and `thinking_tokens` is excluded from derived `total_tokens`.
- **Bounded stream partial-usage observations (`internal/coder`)**: `claude`, `codex`, and `agy` streams deduplicate per-message usage snapshots by validated message ID (`^[A-Za-z0-9_.:-]{1,128}$`, capped at 1024 IDs). When an agent exits without valid terminal usage, incomplete observations (`usage_observation`, `complete: false`, `aggregation: "last_valid_snapshot_per_message_id"`) are written to `<worktree>.diagnostics.json` while canonical `usage` remains nil and cost remains unknown.
- **Bounded verification Git-status snapshots and exclusive sidecars (`internal/coder`)**: Before and after `--verify` execution across all outcomes, bounded `git status --porcelain=v1 -z --untracked-files=all` snapshots (64 KiB, 64 entries, 512-byte path cap, 10-second timeout) are captured while preserving `uncertain` classification on dirty worktrees, exit 77, timeouts, or changed `HEAD`/branch identity. Sidecars (`schema: "irrigator.coder-diagnostics/v1"`) are written exclusively with `O_WRONLY|O_CREATE|O_EXCL` (`0600`) so prior sidecars and symlinks are never overwritten.
- **Zero-copy worktree seeding (`internal/coder`)**: `irrigator code --seed-ignored RELPATH[,RELPATH]` (and `context.seed_ignored`) seeds gitignored directories or files into cell worktrees via copy-on-write clones where supported (`cp -c -R` on macOS APFS, `cp --reflink=auto -a` on Linux), refusing tracked, unignored, symlinked, or repository-escaping paths. See [coding cells](coding-cells.md).
- **Opt-in OS-level verification sandboxing (`internal/coder`)**: `irrigator code --sandbox-verify` (setting `context.sandbox_verify: "true"`) runs `--verify` commands inside `sandbox-exec` on macOS (`darwin`) or `bwrap` on Linux (`linux`) with network disabled and writes confined to the cell worktree, an attempt-scoped temp directory, and `/dev/null`, failing closed with exit `77` (`uncertain`) if the sandbox tool is unavailable. See [coding cells](coding-cells.md).
- **Runtime evidence placement hygiene (`internal/coder`)**: `irrigator code` refuses `--add-dir` paths pointing at or inside `--run` or `--worktrees`, preventing generated runtime evidence or sibling worktrees from leaking into agent reference context. See [coding cells](coding-cells.md).
- **Canonical workflow recipes (`examples/`)**: Reference recipes demonstrating structured multi-stage swarms:
  - [analyze-plan-implement-verify-revise.json](../examples/analyze-plan-implement-verify-revise.json): `analyze -> plan -> implement -> verify -> revise` with shared `interface_packet` scoping and independent `verify_and_review` excluding producer self-assessments.
  - [coding-cell-recipe.json](../examples/coding-cell-recipe.json): isolated coding cell execution with `context` importing `sandbox_verify` and `seed_ignored`, independent verification, and merge gating.
  - [managed-mixed-providers.json](../examples/managed-mixed-providers.json): multi-stage pipeline coordinating planner, implementer, and advisory verification roles with token reporting and budget envelope checks. See [workflow composition](workflow-composition.md).
- **Observability and protocol alignment**: OpenTelemetry `gen_ai.*` span attributes (`internal/telemetry`), explicit MCP `ToolAnnotations` (`internal/mcpserver`), and OSC 7501 `kind=` blocked-state metadata (`internal/terminalview`). See [monitoring](monitoring.md) and [MCP server](mcp.md).

## Local validation scope

- Fake-provider Go unit and race tests (`go test -race ./internal/coder/...`), CLI contract snapshot checks, export hygiene (`make check-exports`), and plugin/documentation contract checks run locally with no network and no live-model verification claimed.
