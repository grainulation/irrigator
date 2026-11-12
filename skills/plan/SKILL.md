---
name: plan
description: Turn a task, claims or an A×D×S shape into a validated, immutable Irrigator swarm/v1 plan file. Plans only; never launches workers.
allowed-tools:
  - Bash
  - Read
  - Glob
  - Write
---

# plan -- Build a validated Irrigator plan

Produce a `swarm/v1` plan file and a sizing/authorization summary. This skill never starts workers and never spends money. Execution belongs to `skills/run/SKILL.md`.

Any host can follow this file as ordinary Markdown. It does not need a host Skill tool: when a sibling skill is named, read its `SKILL.md` by path. The runtime CLI and its plan JSON are the contract; the observer UI is secondary.

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

## Step 0: Locate the CLI (no installation)

```bash
irrigator version
irrigator doctor
```

If `irrigator` is not on PATH, use a built local checkout executable such as `./irrigator`. Building (`make build`) or installing (`make install`) changes the user's machine, so propose it and wait for the user's go-ahead unless the session already authorized that build; `make install` always needs explicit consent. `doctor` reports CLI discovery separately from authentication; finding an executable does not prove login.

Use `irrigator COMMAND --help` for flags. Do not invent flags. Commands accept flags before or after positional arguments.

## Step 1: Exact source preflight

A plan that mentions files, claims or specifications must be built from them as they are now, not from memory.

1. List every source file the plan will depend on, as exact absolute paths. Do not substitute search results for paths the user gave.
2. Read each one. If a path cannot be read, say which exact path failed and why. Never say a file is unreadable without actually trying it.
3. If a shell is available, record `shasum -a 256 <path>` for each file. If not, say hashes were not recorded.
4. Put the paths, hashes and the short extracted facts workers need in plan `context` (named strings). A task selects only the keys it needs through its `context` list.
5. Stop and ask when a required source is missing or stale. Do not plan around a source you have not read.

Grainulator is the evidence layer and this JSON plan is the execution boundary. Translating claims or decisions into tasks is a manual step. There is no automatic Grainulator export or import.

## Step 2: Shape notation, and concurrency apart from it

A shape `A×D×S` (ASCII `x` is also accepted) means:

- **A** root agents; **D** total levels including roots; **S** maximum children per node. The coordinating agent is not counted.
- `W×D` means `W×D×1`.
- Maximum nodes `= A × (1 + S + … + S^(D−1))`; with S=1 it is `A×D`. Examples: `2x5` = 10, `10x2x9` = 100, `25x5x2` = **775**, not 125.
- Plan expansion: A root tasks; `spawn.max_children` = S on every task that may spawn; plan `max_tasks` = the intended bound (at most 1024); `max_depth` = D−1 (at most 16), because roots have depth 0. Leaves must not spawn.
- The maximum is an upper bound. State whether the target is the fully populated tree or a smaller realized graph. Never describe a smaller graph as a completed full tree.
- **Root count does not set concurrency.** `--workers` (1..128) is chosen separately for provider, resources, latency and spend. 25 roots do not imply 25 workers or 775 simultaneous calls.
- Shape depth describes ownership, not the dependency critical path.
- A static list of tasks does not demonstrate recursive delegation. To test it, give parents a `spawn` policy and require real child manifests from the agents.

Preview without launching anything. This command needs a `sizing.v1` calibration file (the Irrigator checkout ships `examples/sizing-shape.json`); if none is available, compute the node counts by hand and say cost is unestimated.

```bash
irrigator size --shape 25x5x2 --sizing examples/sizing-shape.json --budget .25 --backend haiku --json
```

PLAN and `--shape` are mutually exclusive. Read `max_nodes`, the roots-only to full-expansion cost range, recommended workers, `feasible`, `full_expansion_within_budget`, `full_expansion_meets_reliability_floor` and the uncertainty. `feasible` covers admitting roots only; it does not promise the maximum tree can finish. For an existing plan, `irrigator size PLAN --sizing FILE --json` predicts useful parallelism.

## Step 3: Write the plan

Skeleton (adapt; every field below is part of the documented contract):

```json
{
  "api_version": "swarm/v1",
  "name": "short plan name",
  "display": {"version": 1, "publish": false, "title": "<=60 runes", "purpose": "<=140 runes"},
  "max_tasks": 10,
  "max_depth": 1,
  "context": {"sources": "exact paths and hashes from Step 1"},
  "tasks": [
    {
      "id": "draft",
      "prompt": "Self-contained instruction.",
      "depends_on": [],
      "requires": ["brief", "summary"],
      "context": ["sources"],
      "outputs": {"brief": "brief.txt"},
      "checks": {},
      "effort": "low",
      "display": {"title": "<=48", "why_spawned": "<=100", "done_when": "<=140", "outcome_field": "summary"}
    }
  ]
}
```

Rules from the runtime:

- Task IDs are safe identifiers. `depends_on` lists real data dependencies only; independent work stays parallel. Validation rejects cycles, missing dependencies, unsafe IDs and invalid limits.
- `requires` is the exact set of nonempty string output fields. `equals`, and `checks` (`contains`, `min_length`, `go_source`, or a command validator) declare acceptance. `go_source` is syntax only; compilation or behaviour needs a command validator or a separate integration gate. A valid structure does not prove the claims.
- Coding tasks declare `file_fields: ["files"]` together with `requires: ["files"]`; the model returns path/content objects.
- `input_fields` (e.g. `{"build": ["summary"]}`) shrinks inputs; include `files` when source artifacts are needed. It is input minimization, not a sandbox.
- `spawn: {"field": "children", "max_children": S}`: the field holds a JSON string with a `tasks` array. Child IDs become `parent__child`. Children always use the owner's provider. Effort is inherited when omitted; an explicit `low`, `medium` or `high` child effort is accepted only if it is not higher than the owner's (an owner with no effort cannot grant one, and unknown values are rejected). Children cannot broaden context and cannot add command validators. The field may hold `{"tasks":[...]}` or a bare JSON array; the stored raw text is never rewritten and a rejected proposal admits no child. The parent waits for verified children. Defaults are `max_tasks` 128 and `max_depth` 4; maximums are 1024 and 16.
- Named `providers` entries use absolute argument-array commands and `replay: "safe"` or `"manual"` (default). Use `manual` unless re-running the call is truly harmless. Cells are local directories and trusted processes, not sandboxes. A provider may add a container `sandbox` (`docs/sandbox.md`: `image`, `network` default `none`, `mounts`, `env` names); it narrows filesystem and network reach but is not a hardened boundary. Plan `network: "default"` or writable mounts only when the task needs them.
- `effort` is `low`, `medium` or `high` for Haiku.

### First-class public run/task metadata

Give the plan and every task producer-authored `display` metadata. It is not verified, and execution status is reported separately.

- Plan: `{version: 1, publish, title <= 60, purpose <= 140}`. Any other version is rejected.
- Task: `{title <= 48, why_spawned <= 100, done_when <= 140, outcome_field}`. Limits count runes. A nonempty `outcome_field` must name a field in that task's `requires` and must not be a `file_fields` entry.
- No shipped viewer renders display metadata. The fields remain valid schema for compatibility.
- Write real labels: why the task exists, what makes it done, and a short result field. Count the limits before saving. Never put secrets or prompt text in display strings. There is no ETA or capacity field.
- Set `publish` to false unless the user wants metadata visible to an observer.

### Independent acceptance

Do not let the worker that produced a result be its only judge. Add `checks` and command validators written by the planner, or plan a separate `deliver --check` gate. Name in the summary what acceptance will and will not prove.

## Step 4: Preserve plans, never overwrite

- Write to a NEW path. Do not overwrite a plan the user or a run already uses. `irrigator init --out plan.json` also refuses an existing file.
- A run pins the original plan and its hash. Changing unfinished work later goes through `irrigator propose DIR proposal.json` (see `skills/resume/SKILL.md`), not by editing the plan file in place.
- For a revised plan before any run, save a new version such as `plan.v2.json` and keep the old file.
- Starting point: `irrigator init --out NEW_PLAN.json` writes a two-task starter.

## Step 5: Validate

```bash
irrigator plan PLAN.json
irrigator plan PLAN.json --json
```

This validates and previews dependencies; fix every error before reporting success. Check that task and spawn counts fit `max_tasks`/`max_depth`, every display limit holds, and context keys referenced by tasks exist.

## Step 6: Report

Give the user:

- Plan path, task count, and whether it is explicit tasks or a shape expansion.
- Shape `A×D×S`, maximum nodes, intended nodes, `max_tasks`, `max_depth`.
- Concurrency `--workers` and why, separate from shape.
- Backend (`mock`, `haiku` or named provider), per-call `--budget`, and the enforced aggregate spending cap or the statement that none is enforced.
- Source preflight result: paths read, hashes or "not recorded".
- Acceptance design and its limits.
- Whether paid execution is authorized (default: not authorized).

## Paid-use rules

- `--budget` is a per-call limit, not a graph-wide cap. State that every time money is involved.
- Admission against a total needs a configured provider service, such as `irrigator serve --dir DIR --workers N --budget USD` with a named provider that connects to it: it reserves each call's per-call maximum in a durable ledger and refuses calls once spent-or-reserved plus the request exceeds that allocation. That bounds admission reservations; an actual external charge can exceed a reservation, so promise no hard invoice bound. `--sizing` and `provider_balance_usd` are advisory: supplied balances are estimates, not reservations, they only lower local admission, and the provider ledger stays authoritative. User monitoring is not enforcement.
- Do not select a paid backend by default. Offline planning and runs use `--backend mock`; mock results show orchestration only, not model quality or speed.
- Never treat the plan request as authorization to spend. Authorization is the run skill's gate.

## Rules

- Do not run `apply`, `deliver`, `resume`, `retry` or `serve` here.
- Do not send messages, publish, push, or write outside the plan and review files the user asked for.
- Do not claim pricing, a permissions model of Irrigator's own, or a public release. The optional `irrigator mcp` server is read-only unless the user enables `--allow-control`. Local install only, subject to the host's own permission prompts.
- Choosing `--executor local|herdr|tmux` is a run-time decision for the run skill; run `irrigator doctor` to see which are available.

## Next steps

End with exactly two bullet lists, **Auto** and **Manual**. Auto is work already authorized (for example re-validating the plan). Manual is what needs the user: paid-run authorization with an explicit aggregate cap, missing sources, or a decision on shape. Use `None.` when empty. Next: `skills/run/SKILL.md`.

Experimental features (an `experiments` block, such as messaging between tasks or swarms) stay off unless the user asks for them by name. Read `docs/experiments.md` before adding one, and state its risks.
