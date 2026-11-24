---
name: resume
description: Recover an interrupted or failed Irrigator run without blind replay, preserving the immutable plan and verified work.
---

# resume -- Recover an Irrigator run

Use this skill when a durable Irrigator run (a directory containing `state.json`) was interrupted, paused, blocked, has failed nodes, or has nodes in `uncertain` state. It drives the real CLI commands only: `status`, `triage`, `inspect`, `logs`, `resume`, `retry`, `resolve`, `propose`, `decisions`, `pause`, `down`, `cancel`, `export`, `gc`.

This file is plain Markdown. Claude Code, Codex, or a human can follow it manually. It does not assume any host-specific Skill tool. Run commands with the host's shell tool. If the host asks for permission to run a command, respect that answer and report the command as not run.

## Managed execution profiles

For a plan with `execution`, see `docs/execution-profiles.md` before choosing
launch or budget flags. Use named profiles and pinned existing CLI logins; do
not request or copy an API key for `host_session`. Managed plans use profile
`operation_budget_usd` and plan `run_budget_usd`; their run-local ledger enforces
admission reservations. Do not pass `--budget`, replace the backend with mock,
or translate the catalog into external provider commands. Managed helpers and
continuations inherit their owner's profile.

For campaign authority and durable mailbox commands, see `docs/campaigns.md`
and `docs/durable-mailbox.md`. A campaign reserves the full run allowance across
cooperating runs; unknown cost prevents grant closure. Backup restores are
quarantined evidence, never execution authority; see `docs/backup-restore.md`
before copying or restoring a run.

A verified result can have unknown USD cost. Its operation reservation stays
held; token usage does not imply a dollar amount. Report known spend, held
reservations and unknown cost separately. Never promise a final invoice cap,
infer free execution from zero reported spend, release an unknown hold, or
replay an uncertain operation automatically. Existing session authorization
still applies within its stated scope; configuration alone grants none.

## Arguments

$ARGUMENTS

Expected: the run directory (absolute path preferred). If it is missing, ask for it. Do not guess from directory mtimes. If a session registry is in use, read the path from `<registry>` (default `./.irrigator/current-session.json`), which records `run_path` per entry.

## Binary and run preflight

1. Locate the executable: `irrigator` on PATH, or an explicitly named absolute path. Run `irrigator version` and `irrigator doctor`. `doctor` separates CLI discovery from authentication and does not prove login. Record the version in your report.
2. Confirm the run directory exists and holds `state.json`. Read it only through `irrigator status DIR --json`. Never edit `state.json`, `cells/`, leases, or markers by hand.
3. Run `irrigator status DIR` and `irrigator status DIR --json`. Record: control value, per-node status, attempt counts, reported cost, and the backend the run is pinned to. The backend and the original plan are pinned at creation. A resume cannot switch backend or edit the plan.
4. Check whether a controller is already live. OS locks allow one controller per run. `pause` and `down` only acknowledge a request. Wait until the controller has exited before running `resume`. If `status --json` or the registry reports `running`, observe instead (see the observe skill) and do not start a second controller.

## Classify every unfinished node

| Status | Meaning | Action |
|---|---|---|
| `verified` | Accepted result on disk | Reuse. Never rerun. |
| `produced` | Result saved, not yet validated | `resume` validates it without a new model call. |
| `failed` | Failed attempt | Inspect the cause. Replay only when the provider is replay-safe. |
| `uncertain` | Abandoned call or unconfirmed shutdown of a manual provider, or a lost supervisor | **No automatic replay.** See the uncertain protocol. |
| `cancelled` | Cancelled by an operator | Retry explicitly only if the user wants it. |

Inspect each non-verified node with `irrigator inspect DIR NODE` and read events with `irrigator logs DIR --json --since 0` (optionally `--task NODE`). Use only what these commands print. Do not infer node state from chat history.

## Recovery paths

### 1. Plain continuation

```sh
irrigator resume DIR --workers N
```

Accepted nodes are reused. A replacement controller adopts live workers. `--workers` is a hard concurrency ceiling (1..128). It is separate from the plan's shape. A run that already has `RUN/sizing.json` keeps applying it on resume; pass `--sizing FILE` only to replace it deliberately. Sizing is advisory estimates that lower local admission, not a spending reservation. Pass the same `--session-registry ABS` to `apply` and `resume` when several runs share one registry.

### 2. Replay-safe failures

```sh
irrigator resume DIR --retry-failed --workers N
```

Retries only replay-safe failures. A manual provider (the default) is never replayed this way.

### 3. Selected explicit retry

```sh
irrigator retry DIR NODE [--recursive]
```

Starts a new logical operation for selected unfinished nodes. `--recursive` includes descendants. Then run `resume DIR`. Keep the node list small and say which nodes you are retrying and why.

### 4. Uncertain nodes: no blind replay

An `uncertain` node may already have caused an external effect or a charge. Do the following in order:

1. Start with `irrigator triage DIR` (read-only). It shows, per stuck node, whether the provider's exit was confirmed, whether a worker is still live, reported or unknown cost, the last progress lines and, for `irrigator code` cells, the branch, its commits and any uncommitted worktree changes, plus one recommended command. Use `irrigator inspect DIR NODE` and `irrigator logs DIR --task NODE` for more. `irrigator triage DIR --fix` is an interactive walk-through for a person at a terminal; suggest it to the user rather than driving it.
2. Check the external system (files, remote service, spend ledger). Check for surviving provider processes yourself. Recovery never kills a stored PID, and the runtime will not identify processes for you.
3. If the external outcome is known, write the reconciled result as JSON (an object of string fields matching the node's `requires`) to a file you choose. Then run:

   ```sh
   irrigator resolve DIR NODE --result FILE
   ```

   If a supervisor died and shutdown was unconfirmed, add `--confirm-stopped` **only after** the processes are verified stopped, whether by you (the operator can check this itself) or by the user. The flag is a truthful attestation, not necessarily user-provided. It does not stop anything.
4. Only when replay is acceptable and the user has explicitly said so for that operation (an earlier explicit instruction in this session that covers it counts; general run authorization does not resolve uncertainty), run:

   ```sh
   irrigator retry DIR NODE --allow-uncertain --confirm-stopped
   ```

   Replaying can duplicate effects and billing. Stable operation IDs enable provider idempotency but the runner cannot guarantee exactly-once behaviour.

Never pass `--allow-uncertain` without that explicit decision, and never pass `--confirm-stopped` unless you or the user verified the processes stopped. Do not blindly replay. When those conditions hold, proceed without asking again; ask only if replay is not accepted or stoppage cannot be verified.

### 5. Plan changes

The original plan and its hash stay immutable. Work that is still unfinished can be repaired only through a revision proposal:

```sh
irrigator propose DIR proposal.json
irrigator decisions DIR
irrigator resume DIR --workers N
```

The proposal needs `id`, `base_revision` (read it from `decisions`), the complete next active `tasks` list, `reason`, `expected_savings_ms`, `switch_cost_ms`, `extra_cost_usd`, and `confidence` (0 to 1). Verified results are not replayed. Original goals and acceptance checks cannot be removed. Estimates are supplied, not measured. Revisions can add cost: check `extra_cost_usd` against the remaining authorized cap, and ask for authorization only if a paid helper would exceed the existing scope or cap.

## Paid-resume authorization gate

Before any `resume`, `retry`, or revision that can reach a paid backend (the built-in `haiku`, or a named provider that bills), check it against the session's existing authorization (run, backend, cap, reported spend). Inside that scope and cap, proceed without asking and state the items below in your report. Get an explicit yes only when the action exceeds the authorized scope or cap or needs a new run or backend, or while uncertain duplication is unresolved. State:

- the run directory and the backend it is pinned to,
- nodes that would be launched, and how many are retries of earlier spend,
- `--workers` concurrency,
- the **per-call** `--budget` (default 0.25 USD). It is not a graph-wide cap,
- the aggregate spending cap the user authorizes, plus reported spend so far from `status` or `decisions`.

Mock backends and replay-free validation of `produced` nodes do not need a spending authorization. An existing authorization carries over to later retry rounds while total spend (reported plus the worst case of the new round) stays within the authorized cap; a round that would exceed it needs a fresh yes. Unreported charges from interrupted calls can exist, and an actual external charge can exceed a provider reservation. Say so.

## Dispatch evidence

After resuming, record what actually happened from the CLI, not from the exit code alone:

- `irrigator status DIR --json` for control state, node statuses, and reported cost,
- `irrigator logs DIR --json --since LAST_SEQ` for `started`, `produced`, `verified`, `failed` events of the nodes you intended to run,
- `irrigator inspect DIR NODE` for operation and attempt records on any node you retried.

Quote sequence numbers. If a node you intended to run shows no `started` event, say it was not dispatched. Do not claim spawned children exist until an `expanded` event or the graph shows them (`irrigator graph DIR`).

## Independent acceptance

`verified` means the node's declared checks passed. It does not prove the work is correct. Before calling the recovery finished:

1. Confirm `control` is `complete` and every node is `verified`.
2. Export with `irrigator export DIR --dir NEW_DIR` (the directory must not exist) or `--out FILE`.
3. Have a check that did not produce the work, such as an independent test, a `deliver --check` run, or a human review. Report its result separately from the run status. A node's own acceptance does not count as independent acceptance.

If a check fails, report it as an open finding. Do not hide it by retrying until a green result.

## Housekeeping

- `irrigator gc DIR` previews obsolete attempt logs. `--apply --keep 1` deletes them. It needs an idle controller and idle workers, and keeps the newest attempt. Do not run `--apply` unless asked.
- `irrigator pause DIR` drains calls and stops scheduling. `irrigator down DIR` cancels calls and retains progress.
- `irrigator cancel DIR NODE` cancels the node and its descendants.

## Guardrails

- A run's executor (`local`, `herdr` or `tmux`) is recorded and kept by `resume`. `--executor` overrides it and says so; run `irrigator doctor` first. A worker left running by a dead controller keeps its cell lease; wait for it rather than starting a second one. The optional `irrigator mcp` `resume` and `retry_cell` tools (only with `--allow-control`) follow these same rules.

- No remote messages, pushes, or publishing from this skill.
- Do not claim a speedup from concurrency alone. Mock results show orchestration, not model quality.
- Report what you did not test. A source-level fix does not establish that an installed binary behaves the same.
