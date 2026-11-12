---
name: run
description: Execute a validated Irrigator plan with authorized backend, preflight, budget gate, recovery-safe monitoring, dispatch evidence and independent acceptance.
allowed-tools:
  - Bash
  - Read
  - Glob
---

# run -- Execute a plan and report only what happened

Runs an existing plan with the local `irrigator` CLI, watches it, and reports actual evidence. Planning is `skills/plan/SKILL.md`. Recovery details are in `skills/resume/SKILL.md`, observation in `skills/observe/SKILL.md`, spend in `skills/budget/SKILL.md`; read those files by path when needed. This file is ordinary Markdown and needs no host Skill tool.

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

## Gate 1: Preconditions

1. A plan file path exists and validates: `irrigator plan PLAN.json`. If not, use the plan skill first.
2. `irrigator version` and `irrigator doctor` succeed (use `./irrigator` from a local checkout if it is not on PATH). Do not install or build without the user's go-ahead unless the session already authorized that build.
3. Re-run the exact source preflight: read every source path the plan's `context` names and compare recorded hashes (`shasum -a 256`) when the plan recorded them. If a source changed or cannot be read, stop and say which exact path. Do not run against stale sources.
4. Choose a NEW run directory. `apply` and `prepare` refuse to overwrite an existing run; an existing run is continued with `resume`, never re-applied. Never delete or reuse a run directory to "start over"; the original plan and hash stay pinned.

## Inherited launch context

If `IRRIGATOR_LAUNCH_CONTEXT` is present, inspect it with `irrigator context inspect --file "$IRRIGATOR_LAUNCH_CONTEXT"`. Keep the capsule's workspace and registry; changing cwd must not create a new registry. Runtime admission checks the capsule identity and destination. Do not replace or unset it to bypass a mismatch; report which intended launch context is inconsistent. Capsule paths are routing metadata, not additional authorization. See `docs/launch-context.md` for provider receipt semantics. Never describe environment inheritance or a missing acknowledgement as proven agent consumption.

## Gate 2: Backend and paid authorization

- `--backend mock`: free, offline, shows orchestration only. Safe default when the user has not authorized spending.
- `--backend haiku` (needs the Claude CLI) or a named provider may spend money. Proceed only with explicit authorization from the user in this conversation that states: the plan, backend, `--workers`, per-call `--budget`, and the aggregate spending cap. A plan request or silence is not authorization, and an approval for a different plan, backend or cap does not cover this one. An authorization already given in this session for this plan, backend and cap stays valid: do not re-ask for resume, replay-safe retry, build or export inside it. If the cap is missing, ask for it.
- `--budget` is per call, not graph-wide. For unmanaged execution, admission against a total comes from a configured provider service: `irrigator serve --dir DIR --budget USD` with a named provider using `irrigator connect` reserves each call's per-call maximum in a durable ledger and refuses calls once spent-or-reserved plus the request exceeds that allocation. That bounds admission reservations; an actual external charge can exceed a reservation and interrupted calls can go unreported, so promise no hard invoice bound. `--sizing FILE` (with `provider_balance_usd`) is advisory: operator-supplied estimates that only lower local admission (missing `provider_balance_usd` blocks new work); it is not a reservation or aggregate enforcement. If no provider service enforces the cap, tell the user it is only monitored (monitoring is not enforcement), bound the graph with `max_tasks`, and stop when reported cost reaches it.
- Paid retries and repair calls count against the authorized cap. Inside the existing scope and cap they need no new approval; ask only if they would exceed it. Interrupted calls can incur charges absent from reported totals.
- Choose `--workers` independently of root count and shape. State the value and why.
- For a shape-based plan, state roots, levels, branching limit, maximum and intended nodes before launch.

## Step 1: Launch

Save first when you want a pause before any worker starts:

```bash
irrigator prepare PLAN.json --run RUNS/NEW_RUN --backend mock
irrigator resume RUNS/NEW_RUN --workers 2
```

Or create and execute in one command:

```bash
irrigator apply PLAN.json --run RUNS/NEW_RUN --workers 2 --budget 0.25 --backend mock
```

Notes:

- `apply` blocks until the run stops. If the host's command timeout is shorter than the expected run, start it with the host's background facility and redirect output to a file outside the run directory. Ctrl-C or SIGINT requests a draining pause; continue with `resume`, do not launch a second controller. One controller per run is enforced by OS locks.
- Add `--session-registry /absolute/path/registry.json` to `apply`/`resume` when an observer should follow the run; the default is `<cwd>/.irrigator/current-session.json`.
- `--sizing FILE` is opt-in advisory sizing estimates that only lower local admission and is saved as `RUN/sizing.json`; it is not a spending reservation. `--workers` remains a hard ceiling.
- For code delivery use `irrigator deliver PLAN.json --run DIR --dir NEW_DIR --nodes a,b --check '["cmd","arg"]'`. The check is an argument array, runs in the delivery directory, and must not modify delivered source. `--recheck` is needed after failed or interrupted acceptance; a passed check is not rerun, and `deliver` never retries an uncertain node. Graph-verified, exported and accepted are different facts. Read them with `irrigator delivery-status RUN_DIR` (read-only, no model call): `accepted` with acceptance `not_configured` means no check ran, and `stale`, `blocked` or `reconcile_external` mean stop and inspect, not deliver again.

## Step 2: Watch (read-only)

```bash
irrigator status RUNS/NEW_RUN --json
irrigator logs RUNS/NEW_RUN --json --since 0
irrigator inspect RUNS/NEW_RUN NODE
irrigator graph RUNS/NEW_RUN
```

Watch progress in the terminal with `irrigator status RUNS/NEW_RUN --watch`, and print dependencies with `irrigator graph RUNS/NEW_RUN`. These make no model calls. Planner `display` metadata is plan-authored text and is not an observation of the run.

Judge the run from `status`, not just the exit code.

## Step 3: Failures and uncertainty (no blind replay)

- `pause DIR` drains calls and stops scheduling; `down DIR` cancels calls and keeps progress. Wait for the controller to exit before resuming.
- A manual-replay provider call that was abandoned becomes `uncertain`. Do NOT run `resume --retry-failed` or `retry --allow-uncertain` to make it go away. First inspect the external system and any surviving process, then either `resolve DIR NODE --result FILE [--confirm-stopped]` with a reconciled result, or replay only after the user has explicitly accepted possible duplication for that operation (a prior explicit instruction in this session that covers it counts). `--confirm-stopped` is a truthful operator attestation that survivors are stopped, not necessarily user-provided: you may pass it after verifying yourself. It does not kill processes.
- `resume --retry-failed` only retries replay-safe failures; paid retries inside the existing authorized scope and cap need no new approval. `retry DIR NODE [--recursive]` is an explicit new logical operation for selected nodes. `cancel DIR NODE` includes descendants.
- Accepted nodes are reused, not rerun. To change unfinished work, use `propose DIR FILE` then `resume`; never edit the original plan or state files. Revisions need the current `base_revision`; stale ones are rejected.
- Never delete `state.json` or cell directories. `gc DIR` previews by default; `--apply` is only for the user's cleanup request.

## Step 4: Independent acceptance

A run in which every node is `verified` shows that the graph's declared checks passed. It does not show that the claims or code are right.

- Export only verified results to a NEW location: `irrigator export RUNS/NEW_RUN --dir NEW_DIR` (or `--out FILE`, not both). Exporting accepted results needs no new approval.
- Run an acceptance check that the producing workers did not author (the plan's validator, a `deliver --check`, or tests/review you run yourself). Report its command and result.
- If no independent check exists, say "graph-verified only, not independently accepted".

## Step 5: Dispatch evidence report

Report from the run's records, not from the plan:

- Run directory, plan name and hash, backend, `--workers`, per-call budget, aggregate cap and how it was enforced.
- Actual node count versus the declared maximum; verified, failed, cancelled, uncertain counts. State plainly if the graph is smaller than the planned shape.
- Spawn evidence: actual `parent__child` nodes in `status --json` or `graph`, observed levels and branching. A task list written in the plan is not delegation evidence.
- Attempts and retries per node (from `inspect`/`status --json`) and the reported total cost. For mock runs cost is not model spend.
- Concurrency: only claim a peak if you computed it from event timestamps in `logs --json`; otherwise say it was not measured.
- Whether acceptance was independent (Step 4).
- Known limits: reported cost excludes unreported charges from interrupted calls; mock results are not model quality or speed evidence.

## Rules

- Never spend without the Gate 2 authorization. Never claim a graph-wide cap the runtime does not enforce.
- Never replay an uncertain outcome blindly. Never overwrite or edit a saved plan, run, or state.
- Do not push, publish, message anyone, or touch remote systems. Do not claim pricing, release, or permissions beyond the host's own.
- **Executor.** Run `irrigator doctor` first. `apply`/`resume` take `--executor local|herdr|tmux` (default `local`); it is recorded per run and shown by `irrigator status`. To watch a worker: `tmux attach -t irrigator-<run8>` or `hc attach <terminal>`; detach, never type into it.
- **Sandbox.** A provider `sandbox` (`docs/sandbox.md`) narrows filesystem and network reach; it is not a security boundary, and cells without it are not sandboxes. Do not describe a run as isolated beyond that.
- **Approvals.** For a blocked request use `irrigator approvals link --run DIR --request ID --action approve`, `approvals serve --run DIR [--expose cloudflare|tailscale]` or `approvals notify --run DIR --request ID --via ntfy`. Exposing or notifying leaves the machine: only on the user's request, and the decision is theirs.
- **MCP.** With `irrigator mcp` connected, `run_status` and `list_runs` report the executor. Its `apply` needs `--allow-control` and follows the same budget and backend gate; `executor` is an optional argument.
- Observation and reporting do not change the run.

## Next steps

End with exactly two bullet lists, **Auto** and **Manual**. Auto: work already authorized (status check, export, acceptance command). Manual: only decisions, access or authorization the user must give (a retry or spend beyond the authorized scope or cap, a new scope, a decision to replay an uncertain outcome). Use `None.` when empty.
