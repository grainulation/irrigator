---
name: router
description: Route a plain-language Irrigator request to the right workflow skill (plan, run, resume, observe, budget) or to the main irrigator skill. Use when the user's intent about durable local agent graphs is unclear.
---

# Router

Maps a user's message to an Irrigator workflow. This is a Markdown decision table. It works in Codex and any host without a slash-command runner: open the target `skills/<name>/SKILL.md` and follow it. If the host does offer a skill-invocation mechanism, it may be used, but nothing here depends on it.

The skills are `irrigator` (general workflow), `swarm` (swarm-first front door, see below), `plan`, `run`, `resume`, `observe`, `debug`, `budget`. Verify the file exists before routing. If it does not, say so and apply `skills/irrigator/SKILL.md`.

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

## Intent map

| User says something like… | Skill | Main CLI commands |
| --- | --- | --- |
| "design a swarm", "split this into agents", "turn this into a plan", "25x5x2" | `plan` | `init`, `plan`, `size --shape` |
| "how many agents", "what would this cost", "how many workers", "is the budget enough" | `budget` | `size`, `size --shape`, `decisions` |
| "run it", "launch", "apply the plan", "deliver these files" | `run` | `prepare`, `apply`, `deliver` |
| "have agents fix/implement this in my repo", "run Claude/Codex/agy on these tasks", "one branch per task" | `run` (coding tasks) | `code --plan-only`, `code`, `review list`, `code cleanup` |
| "it crashed", "continue", "retry the failed node", "it says uncertain" | `resume` | `resume`, `inspect`, `retry`, `resolve`, `cancel` |
| "where are we", "show progress", "watch it", "open the graph" | `observe` | `status --watch`, `logs`, `graph` |
| "why is it stuck", "diagnose this run", "what failed" | `debug` (read-only) | `debug`, `status`, `inspect`, `logs` |
| "pause it", "stop spending" | `resume` (control) | `pause`, `down` |
| "export the results", "clean up logs" | `irrigator` | `export`, `gc` (preview first) |
| "is it installed", "try it", "demo" | `irrigator` | `doctor`, `version`, `demo` |
| "revise the graph", "add helpers to the running graph" | `plan` (revision), then `run` or `resume` | `propose`, `decisions`, `policy` |
| "compose reusable workflows" | `plan`; read `docs/workflow-composition.md` | `compose` (offline compilation only) |
| "back up this run", "restore evidence" | `irrigator`; read `docs/backup-restore.md` | `backup create`, `backup verify`, `backup restore` (quarantined evidence) |
| "share admission allowance across runs" | `budget`; read `docs/campaigns.md` | `campaign init`, `campaign admit`, `campaign status`, `campaign close` |
| "queue a task signal or cancellation", "acknowledge a notification" | `resume`; read `docs/durable-mailbox.md` | `mailbox enqueue`, `mailbox process`, `mailbox outbox`, `mailbox ack` (v5 only) |
| "retain accepted history across managed turns" | `plan`; read `docs/managed-history.md` | v5 profile `explicit_history` opt-in; same-agent accepted history bounded by current authority, no model/provider/session switching |

If several rows match, choose the earliest stage the user has not completed: plan → budget → run → observe → resume. Say which skill you chose and why, in one sentence.

**Front door.** `skills/swarm/SKILL.md` is an optional swarm-first entry. It applies this same map and also accepts a leading stage word (`plan`, `budget`, `run`, `resume`, `observe`). It adds no routes, gates or commands. If you arrived from `swarm`, do not hand back to it: route with this table. If the user names `swarm` but gives no run directory, plan file or shape, ask the one short question described in that skill.

## Always-on rules (apply whichever skill you route to)

1. **Shape is not concurrency.** `AxDxS` = A roots, D levels including roots, S max children per node; max nodes = A·Σ S^k for k=0..D−1 (`25x5x2` = 775). Workers are a separate `--workers` choice, and money is a third. Do not assume a worker count from a shape.
2. **Run and task metadata is public.** Titles, purposes, why-spawned, done-when and outcome fields belong in the plan before admission. Do not invent them from IDs afterward.
3. **Preflight with exact sources.** Read the exact files the user named from their exact paths. Do not substitute near matches. Missing input is a stop, not a guess.
4. **Paid work needs explicit authorization.** State roots, levels, max children, max and intended tasks, workers, per-call limit and aggregate cap, and get the user's explicit yes first. That authorization persists for the session within its scope and cap: resume, replay-safe retry, build and export inside it need no fresh yes; ask only for a new scope or a higher cap. `--budget` is per call, not a graph-wide cap, and `--sizing` is advisory estimates; for unmanaged plans, a configured provider service (`serve --budget`) enforces admission against its allocation, and that does not bound the final external charge. If no cost authorization exists, use `--backend mock`, `prepare`, `size` or `demo`, which are free.
5. **No blind replay.** Uncertain nodes need `inspect` and `resolve`, or an explicit decision accepting duplication before `retry --allow-uncertain`. `resolve --confirm-stopped` and `retry --confirm-stopped` are truthful attestations that you may give once you verified the processes are stopped. Never delete state to force a rerun.
6. **Keep the plan immutable.** Revisions go through `propose`, not editing the admitted plan.
7. **Dispatch evidence.** Do not call work executing from a process or controller `running` alone. Read `status --json`, `inspect` or `logs` and report their age.
8. **Independent acceptance.** Verified is not correct. A reviewer cannot be the author.
9. **Local and host-respecting.** No remote messages, publication, or installation without a request. Respect host permission prompts. The optional `irrigator mcp` server (`docs/mcp.md`) is read-only unless the user enabled `--allow-control`; the CLI is always sufficient. Executors (`--executor local|herdr|tmux`, check with `irrigator doctor`), the provider container `sandbox` and `irrigator approvals` are covered in `skills/irrigator/SKILL.md` section 10.

## Instructions

1. Read the user's message and the active task for intent signals: verbs, run directories, plan paths, shape strings like `AxDxS`, dollar amounts, node IDs.
2. If the match is clear, announce it plainly. Example: *Following `skills/plan/SKILL.md`: this designs the graph and runs only free checks. Say "just answer" if you wanted a quick reply instead.*
3. Open the target skill and follow it. Keep the active task and use context to resolve routine ambiguity. Ask only for information that changes the work, such as authorization for paid spend, which run directory, or which node.
4. An explicit request to deliver software through Irrigator routes through `plan` then `run`, and the job includes acceptance and verification, not just a launch.

## When not to route

- Questions about how Irrigator works: answer from the docs and, where helpful, from `irrigator --help`.
- Ordinary code edits that did not ask for an Irrigator run.
- Grainulator evidence-sprint work; it is not an Irrigator feature.
- General conversation, or ambiguous intent with no run, plan or shape context: ask one short question.

## Next steps

When reporting, end with up to three concrete next steps split into **Auto** (work the agent may continue under existing authorization) and **Manual** (needs the user's decision, spend authorization or access). Use `None.` for an empty group. A suggestion never grants permission; paid execution is Manual until the user authorizes it, and work inside an existing authorization (scope and cap) is Auto. Do not invent work to fill the list. Continue authorized Auto work without asking again.
