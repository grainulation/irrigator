---
name: observe
description: Read-only inspection of Irrigator runs via status, logs, graph, inspect and the optional read-only MCP server, without changing the run.
---

# observe -- Watch an Irrigator run

Use this skill to answer "what is the run doing?" without altering it. The runtime CLI is the primary source of truth, and the terminal is the interface.

This file is plain Markdown for Claude Code, Codex, or a human. Run the commands with the host shell. Respect permission prompts. If a command is declined, say it was not run.

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

Expected: a run directory (absolute path preferred). Without one, ask. Do not infer the current run from directory timestamps. If using the session registry, read it explicitly (see below).

## Read-only command set

Only these commands belong in an observation pass. None of them creates tasks, resumes work, or makes model calls.

```sh
irrigator version
irrigator status DIR                    # human summary
irrigator status DIR --json             # full state
irrigator status DIR --watch [--interval 250ms]
irrigator inspect DIR NODE              # node, operation, attempts
irrigator graph DIR                     # Mermaid dependencies and ownership
irrigator logs DIR --json --since N [--task NODE] [--follow]
irrigator decisions DIR                 # revisions, proposals, reported cost
irrigator plan PLAN [--json]            # validate/preview a plan file, no run
```

`irrigator status` also prints the run's executor (`executor` in `--json`). To see a worker running under `tmux` or `herdr`, attach read-only in spirit: `tmux attach -t irrigator-<run8>` or `hc attach <terminal>`, detach with the executor's key, and never type into the worker (`docs/executors.md`). If the host has the optional `irrigator mcp` server connected, `list_runs`, `run_status`, `run_logs` and `inspect_cell` return the same read-only data; its control tools exist only with `--allow-control` and are not part of observation.

`irrigator debug DIR [--json] [--node ID]` (bounded diagnosis, see `skills/debug/SKILL.md`) and `irrigator delivery-status RUN_DIR` (delivery stage, acceptance and recovery enums only) are also read-only and make no model call. `irrigator events DIR` and `irrigator artifacts DIR` also exist but are not listed in the CLI help. `artifacts` prints verified result contents, which can include prompts' outputs or sensitive material. Use it only when the user asks for contents. Prefer `inspect`.

Do **not** use `resume`, `retry`, `resolve`, `cancel`, `pause`, `down`, `propose`, `policy`, `export`, `gc --apply`, `apply`, `deliver`, or `serve` during observation. If one is needed, switch to the resume skill and its authorization gates.

## Reading the output

- Report only what the CLI printed. Quote node IDs, status strings, sequence numbers, and `reported_cost_usd` as given.
- Node outcomes include `verified`, `produced`, `failed`, `uncertain`, and `cancelled`. Only `verified` results release dependents and count as accepted. `produced` is saved but not yet validated.
- `status --watch` ends when the run reports `complete` or `blocked`, or when control is not `run` and no workers hold leases.
- Reported cost is what providers reported. Interrupted calls can add charges that are not in the total.
- There is no ETA or speedup measurement in the runtime. Do not invent either. Concurrency alone does not demonstrate a speedup.
- Shape (A×D×S) describes ownership, not how many workers are running. Read the real worker and node counts from status.
- Spawned children show up as `expanded` events and as `parent__child` node IDs. If you cannot see them in `graph` or `status`, they do not exist yet.

### Derived session status (registry)

A session registry reader derives a run's state from disk, never from chat (see `docs/session-registry-v2.md`):

`complete` (control=complete, every node verified, no live lease), `inconsistent` (control=complete otherwise), `running` (live lease, or control=run with a held controller lock), `stalled` (control=run, no controller), `paused`, `blocked`, `ready`, `indeterminate` (lock probe error), `missing`, `unavailable`.

A `stalled` run needs a recovery decision, so hand off to the resume skill. Do not resume from here.

### Event log limits

`irrigator events DIR` and any bounded event tail are windows, not complete history. Sequence gaps are normal filtering. Use `irrigator logs DIR --json --since 0` for the full sequenced log.

## Terminal views

The terminal is the interface on every platform: `irrigator status DIR --watch`, `graph`, `logs` and `inspect`. For local changes awaiting review, `irrigator review list` and `irrigator review diff` provide read-only views. The interactive review TUI can record approvals and change requests, so launch it only for an explicit review request. `irrigator session list` and `irrigator session show SESSION_ID` summarize agent sessions (prompts, tool activity, replies) and are separate from run state.

### Session registry

`apply` and `resume` publish to the registry given by `--session-registry ABS` (default `./.irrigator/current-session.json`). Version 2 holds up to 16 recent and 32 active entries. A missing, malformed, or mismatched registry reads as `control: "unavailable"`, and a reader does not fall back to an older run. If the registry says `unavailable`, check the registry path and the run path with `status DIR` before concluding anything about the run.

## Report format

Keep the report short and evidence-based:

1. Run directory, binary version, time of observation.
2. Control state and counts by node status (from `status --json`).
3. Nodes needing attention (`failed`, `uncertain`, `cancelled`, or stuck without a controller), each with the next CLI command **name** and the skill that owns it.
4. Reported cost so far, with the caveat about unreported charges.
5. What you did not observe (for example, the graph was not printed, or logs after sequence N were not read).

Do not send messages to remote services, and do not change the run in the course of observing it.
