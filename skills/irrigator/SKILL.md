---
name: irrigator
description: Use Irrigator to plan, size, run, observe, recover and accept durable local agent graphs. Use when the user names Irrigator, names an A×D×S swarm shape, or asks for a tracked parallel agent run with acceptance checks.
---

For a first run, follow `docs/first-run.md`. Start with mock workers and inspect
the result before configuring providers. Unmanaged plans require an explicit
`--backend`; `--call-budget` is a per-call limit (`--budget` is an alias).


# Irrigator

Irrigator turns an explicit JSON task graph (`swarm/v1`) into bounded parallel agent work. It keeps verified results on disk and resumes after interruption. It is a local Go CLI (`irrigator`). This skill is a manual Markdown workflow: follow it with whatever shell and file tools the host provides. Do not assume a Claude-only `Skill` tool or slash-command runner. If the host has the optional `irrigator mcp` server connected, its tools are a read-only shortcut for the same state; the CLI works without it.

Preserve the user's requested outcome. If they asked for delivered work, the job ends with accepted artifacts and evidence, not a launched run.

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

## Sibling skills

Read the sibling file directly when the host cannot invoke it by name. Paths are relative to this skill's plugin or checkout root.

| Need | Skill file |
| --- | --- |
| Start from the swarm-first front door | `skills/swarm/SKILL.md` |
| Decide which workflow applies | `skills/router/SKILL.md` |
| Design a plan, shape and metadata | `skills/plan/SKILL.md` |
| Admit and execute a plan | `skills/run/SKILL.md` |
| Recover, retry, resolve uncertainty | `skills/resume/SKILL.md` |
| Watch a run, read the observer | `skills/observe/SKILL.md` |
| Diagnose a stuck, failed or uncertain run (read-only) | `skills/debug/SKILL.md` |
| Size and authorize spend | `skills/budget/SKILL.md` |

If a sibling file is missing, say so and apply the rules in this file. Do not invent its contents.

## 1. Locate the runtime

1. Find the executable: `irrigator` on PATH, or a binary the user built in their checkout (`make build` produces `./irrigator`). Do not build, install or copy a binary into a directory unless the user asked or the session already authorized that build; `make install` writes to `~/.local/bin` and always needs explicit consent.
2. Run `irrigator doctor` (JSON) and `irrigator version`. Report what they actually say. `doctor` distinguishes finding the Claude CLI from being authenticated; finding an executable does not prove a login.
3. Free, credential-free checks that need no authorization: `irrigator demo [--run NEW_DIR]`, `--backend mock`, `irrigator plan`, `irrigator size`.
4. If the binary is missing, report that and the exact way the user can build it from a source checkout (`make build`, needs Go). The plugin is source-only: it ships skills and docs, not an `irrigator` executable, and nothing here installs one. Never claim the runtime works without running it.

Command names in this workflow are the real CLI: `init`, `demo`, `review`, `session`, `mcp`, `plan`, `size`, `prepare`, `apply`, `deliver`, `resume`, `status`, `inspect`, `graph`, `propose`, `decisions`, `policy`, `logs`, `pause`, `down`, `cancel`, `retry`, `resolve`, `export`, `gc`, `serve`, `connect`, `doctor`, `version`. If `irrigator --help` disagrees with this file, trust the binary and tell the user.

## 2. Shape is not concurrency, and neither is money

Use **A×D×S** (or `AxDxS`): A root agents, D total levels **including** roots, at most S children per node. The coordinator is excluded. `W×D` means `W×D×1`.

- Maximum nodes = A × (1 + S + … + S^(D−1)); with S=1 this is A×D. `2x5` = 10, `10x2x9` = 100, `25x5x2` = **775**, not 125. The runtime bound is 1024 nodes and D ≤ 16.
- A shape is a ceiling, not a promise of expansion and not an instruction to fill every slot.
- `--workers` is separate (1..128). Roots do not set it. Waiting ancestors do not occupy worker capacity. Never infer a speedup from worker count.
- Preview a shape with no provider calls: `irrigator size --shape AxDxS --sizing FILE --budget USD --backend NAME [--json]`. `PLAN` and `--shape` are mutually exclusive. Depth is ownership depth, not the dependency critical path. Read `feasible`, `full_expansion_within_budget` and `full_expansion_meets_reliability_floor` separately.
- Expand a shape into an explicit plan: A root tasks, `spawn.max_children` = S on eligible nodes, `max_depth` = D−1 (roots are depth 0), `max_tasks` sized to the intended graph (defaults 128/4, maximums 1024/16). Leaves must not spawn. A static task list does not demonstrate agent spawning; recursive tests need real child manifests from the agents.
- Declare `observer_shape` (`roots`, `levels`, `max_children`) when the structure is known. It is read-only provenance and grants no rights. Validation rejects an initially invalid shape at plan load (non-positive or oversized values, `roots` not equal to the root task count, `levels`-1 above the effective `max_depth`, a root without `spawn` when levels > 1, or a root spawn cap above `max_children`). Only a persisted or revised actual topology that later diverges from a valid shape makes the observer report capacity unknown. Unknown capacity is allowed only with a stated reason.

## 3. Public run and task metadata is part of the plan

Author it **before** the plan is admitted. Do not derive it later from prompts or IDs. Plan `display` is `{version:1, publish, title≤60, purpose≤140}`. Each task `display` is `{title≤48, why_spawned≤100, done_when≤140, outcome_field}`, counted in runes. A non-empty `outcome_field` must name a `requires` field and must not be a `file_fields` entry.

Write them for a reader who never saw this conversation. Check wording by reading it; a non-empty-string check cannot judge usefulness. The `display` fields stay valid plan schema for compatibility; treat them as plan-authored text that no shipped viewer renders. Do not rewrite an admitted plan to retrofit labels; correct the next launch, or use a supported revision.

If the checkout provides `tools/launch_metadata_preflight.py`, run it as an extra check. Otherwise run `irrigator plan PLAN --json` and review manually. State which one actually ran.

## 4. Preflight (a failed preflight is a launch failure, not a running agent)

Before any paid launch, verify and record:

1. **Exact source identity.** The plan file path and content you will pass, and any input files it relies on, read from their exact absolute paths. Confirm they exist, are non-empty and readable. Do not substitute a similar file, a summary, or memory. If an input cannot be read from its authorized path, report that and stop.
2. **Plan validity:** `irrigator plan PLAN` (add `--json` to see structure). Plan validation rejects cycles, missing dependencies, unsafe IDs and invalid limits.
3. **Unique new run directory.** `apply` and `prepare` do not overwrite. Check for duplicate running operations on the same run. A matching `--run` for `deliver` resumes.
4. **Provider availability:** `doctor` for Haiku. Absolute executable paths for named providers. Never claim a login was verified.
5. **Output paths** are unique and inside the intended directory.
6. **Free dry run first** where possible: `--backend mock`, `prepare`, `size`.
7. **Concurrency and budget statement** (next section).

## 5. Paid budget requires explicit authorization

CLI `--budget` is a **per-call** limit, not a graph-wide cap. Defaults ($0.25 per call, 2 workers) are not authorization.

Before a paid run, state to the user: roots, levels, max children, **maximum** and **intended** task counts, `--workers`, per-call limit, and the aggregate spending cap. Count retries and extra reviews in actual spend. Then obtain the user's explicit authorization for that amount in the conversation. A shape never authorizes spending; a user mentioning a shape is not an authorization. Once given, that authorization covers the stated plan, backend and cap and persists for the session: resume, replay-safe retry, build and export that stay inside that scope and cap need no fresh approval, and you continue them without asking. Ask again only when an action would exceed the authorized scope or cap (a new plan or backend, spend beyond the cap) or while an uncertain operation remains unresolved. It is not a standing default for a new scope or a higher cap. Do not silently switch models or claim an unavailable model ran. Use `--backend mock` for routine verification.

Keep four numbers apart: **estimate** (sizing output such as `expected_cost_usd`, and operator-supplied `provider_balance_usd`), **allocation** (the configured total, such as `serve --budget USD`), **reservation** (the per-call maximum that `serve` holds in its durable ledger while a call is pending or uncertain; it refuses a new call when spent-or-reserved plus the request would exceed the allocation) and **reported cost** (what the provider returned; for a completed call it replaces the reservation). Report separately: authorized ceiling, held reservations, reported spend, uncertain cost. Reported cost can omit charges from interrupted calls, and an actual external charge can exceed a reservation, so never promise a hard invoice bound. `--sizing FILE` is advisory: it only lowers local admission from estimates (it fails closed if `provider_balance_usd` is missing) and is neither a reservation nor aggregate enforcement. The configured provider service ledger enforces admission against its allocation. A user watching spend is monitoring, not enforcement.

## 6. Preserve the immutable plan

The original plan and its hash never change. Do not edit a plan file after admission to "fix" a run. Changes to unfinished work go through `irrigator propose DIR FILE` (complete next active task list, `base_revision`, `reason`, estimates) and are evaluated by a running controller or by `irrigator resume DIR`. Inspect with `irrigator decisions DIR`. Proposals cannot remove original goals or acceptance checks, and decision scores use supplied estimates, not learned probabilities. Keep verified results, acceptance contracts, historical attempts, archived costs and uncertain-effect protections.

## 7. Never replay uncertain work blindly

A `manual` provider's abandoned call becomes `uncertain`. So does any node whose provider process shutdown was not confirmed. Do not run `retry --allow-uncertain` merely to get unstuck. Instead:

1. `irrigator inspect DIR NODE`; inspect the external system and any surviving processes. Recovery never kills a stored PID.
2. Check for surviving processes yourself and stop them if you can; ask the user only when you lack access.
3. If the effect already completed, record it: `irrigator resolve DIR NODE --result FILE --confirm-stopped`. That flag is a truthful operator attestation that surviving processes are stopped, not necessarily user-provided; it kills nothing. You can verify that yourself and pass it once verified. Do not pass it unverified.
4. Replay only when the user has explicitly accepted possible duplication for that operation (an earlier explicit instruction in this session that covers it counts; general run authorization does not resolve uncertainty): `irrigator retry DIR NODE --allow-uncertain --confirm-stopped`.

`resume DIR --retry-failed` retries replay-safe failures only and needs no fresh approval inside the existing authorization. Never delete receipts, state or attempt logs to force a replay. Run `gc` only on an idle run, preview first, and `--apply` only with consent.

## 8. Actual dispatch evidence

A run is **executing** only with execution evidence you read: `irrigator status DIR --json`, `inspect`, `logs DIR --json`, or the provider's output/metrics. A controller saying `running`, a launched process, or an agent's own claim is not model dispatch evidence. Report the age of each observation. If a ready task is not dispatched, give the reason: worker cap, dependency, write conflict, provider outage, budget limit, or user pause. Do not call a run "working" when nothing has been observed. The terminal dashboard (`status --watch`) is read-only and secondary to the durable state; the runner being active and a watcher being open are separate facts. Never fabricate activity.

To follow several runs, pass the same absolute path as `--session-registry ABS` to `apply` and `resume`, and confirm the registry entry carries the exact run identity.

## 9. Independent acceptance

Model completion, verified-node status, correctness review and deployment are different milestones. A `verified` node means its declared checks passed, not that its claims or code are correct. `go_source` checks only syntax. Compilation and behaviour need a command validator or a separate integration gate. For `deliver`, pass `--check` as a JSON argv array; without it, acceptance means graph verification and assembly only. Reviewers must be independent of what they review; self-review is not independent. Close a failure only by re-running the original failing path on the affected artifact. Report untested layers.

Export accepted work with `irrigator export DIR --dir NEW_DIR` (destination must not exist) or `--out FILE`; inside the existing authorization this needs no new approval.

## 10. Executors, sandbox, approvals, MCP

- **Executor.** `apply`, `prepare`, `resume` and `deliver` accept `--executor local|herdr|tmux` (default `local`; `IRRIGATOR_EXECUTOR` also works). The choice is recorded in the run and shown by `irrigator status` (text and `--json`); a resume keeps it unless you pass `--executor`. `herdr` and `tmux` are opt-in and only change where each worker process runs. Watch with `tmux attach -t irrigator-<run8>` or `hc attach <terminal>`; detaching leaves the worker running, and you must not type into it. Details: `docs/executors.md`.
- **Check first.** `irrigator doctor` lists each executor under `executors` as OK, too old, or not installed. A missing herdr or tmux is not an error; do not select an executor `doctor` does not report OK.
- **Container sandbox.** A command provider may add a `sandbox` object (`docs/sandbox.md`). It limits filesystem and network reach to the cell directory, listed mounts and the chosen network (default `none`). It is a convenience boundary, not a hardened one: mounts, `network: "default"` and passed `env` are exposed, and a hostile image or runtime is out of scope. Without a `sandbox`, cells are still trusted local directories, not sandboxes.
- **Approvals.** When a worker blocks on a request, a person can decide from a phone: `irrigator approvals link --run DIR --request ID --action approve --ttl 5m` prints a one-use link; `approvals serve --run DIR [--expose cloudflare|tailscale]` serves it (loopback unless `--expose`); `approvals notify --run DIR --request ID --via ntfy|webhook` sends it. Exposing or notifying reaches outside the machine, so do it only when the user asks. Never approve on the user's behalf. An approval records a workflow decision for a head; it is not a security boundary and it does not merge anything. See `docs/approvals.md`.
- **MCP.** `irrigator mcp` is a stdio server (`docs/mcp.md`). It is read-only by default (`list_runs`, `run_status`, `run_logs`, `inspect_cell`, `review_list`, `validate_plan`). Control tools (`apply`, `pause`, `resume`, `retry_cell`, `decide_approval`) exist only if the user started it with `--allow-control`; they obey the same budget, backend and uncertainty rules as the CLI. The Claude plugin registers the read-only server automatically; for Codex, add `[mcp_servers.irrigator]` with `command = "irrigator"` and `args = ["mcp"]` to `~/.codex/config.toml`, only if the user asks.

## 11. Boundaries

- Local files and local processes only. A git worktree isolates concurrent checkouts; it is not a host security sandbox. Dependencies pass result data only; git preparation, acceptance and settlement are explicit steps. Cells are trusted local directories, not sandboxes unless a provider sets a container `sandbox`, and even then only as described above.
- No push, publish, deployment, remote install, or messages to other people unless the user asks for that action. Do not send status updates automatically.
- Respect the host's permission prompts. Do not bypass them, and do not write run state files by hand.
- Compatibility names stay as they are: `swarm/v1`, `swarm.worker/v1`, `SWARM_*`.

## Final report

Give: artifact paths, commands the user can re-run, checks that actually ran and their scope, source identity, actual nodes/model calls/retries, reported cost and uncertainty, and remaining gaps. Intended and materialized shape are reported separately. If a graph only partly expanded, say so.
