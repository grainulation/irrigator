---
name: budget
description: Size an Irrigator shape and get explicit paid-spend authorization before any model-backed run, retry, or revision.
---

# budget -- Size and authorize Irrigator spending

Use this skill before any paid execution, and any time the user asks "how many agents" or "what will it cost". It uses the real `irrigator size` dry run and the run's reported ledger. It states no prices or token rates of its own. Costs come only from a sizing file the user supplies, or from reported run data.

This is plain Markdown that Claude Code, Codex, or a human can follow manually. Use the host shell for commands and respect permission prompts.

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
held; token usage does not imply a dollar amount. Report known reported spend,
unknown-cost operations, held reservations, and token totals separately. A cell
that reported tokens without a dollar cost has cost basis `unknown` and is never
presented as $0. Never promise a final invoice cap, infer free execution from
zero reported spend, release an unknown hold, or replay an uncertain operation
automatically. Existing session authorization still applies within its stated
scope; configuration alone grants none.

## Arguments

$ARGUMENTS

## Terms (do not mix them up)

- **Shape `A×D×S`** (also `AxDxS`): A root agents, D total levels including roots (at most 16), at most S children per node. The coordinator is excluded. `W×D` means `W×D×1`.
- **Maximum nodes** = A × (S^0 + S^1 + ... + S^(D-1)). With S=1 this is A×D. Examples: `1x1x1` = 1, `2x5` = 10, `25x5x2` = **775**, not 125. The tool bound is 1024 nodes. This is an upper bound, not a promised expansion.
- **Intended tasks**: the number you actually plan to run. It may be well below the maximum.
- **Concurrency**: `--workers` (1..128). It is separate from the shape. Waiting parents do not occupy worker capacity. More workers does not mean more agents, and more agents does not mean more concurrency.
- **Per-call budget**: `--budget USD` (default 0.25). It limits a single call. It is **not** a graph-wide cap.
- **Aggregate cap**: the total the user is willing to spend. The CLI has no flag that enforces it for `apply`. You must state it and track reported spend against it yourself, unless a configured provider service ledger enforces admission against it (see `serve`).
- **Estimate, allocation, reservation, reported cost**: an *estimate* is a sizing number (`expected_cost_usd`, supplied `provider_balance_usd`, `budget_usd`) and is advisory. An *allocation* is the configured total, such as `serve --budget USD`. A *reservation* is the per-call maximum the provider service holds in its ledger while a call is pending or uncertain; new calls are refused when spent-or-reserved plus the request would exceed the allocation. *Reported cost* is what the provider returned; on completion it replaces the reservation. Reservations bound admission, not the final external charge, which can exceed them.
- A shape never authorizes spending. Ownership depth is not the dependency critical path.

## Step 1: Dry-run sizing (free, no model calls)

```sh
irrigator size --shape 25x5x2 --sizing SIZING.json --budget .25 --backend haiku [--json]
irrigator size PLAN --sizing SIZING.json --json
```

`PLAN` and `--shape` are mutually exclusive. The command runs no work and changes no plan. Read these from the output (use `--json` for exact keys): maximum nodes, the cost range from roots-only to full expansion, predicted versus recommended workers, `feasible`, `full_expansion_within_budget`, `full_expansion_meets_reliability_floor`, and the uncertainty notes.

Caveats to relay:

- `feasible` covers admitting the roots only. It does not promise the full tree can finish.
- Provider balances and calibration in the sizing file are operator-supplied estimates, not fetched balances. The durable provider ledger remains authoritative.
- At runtime, a sizing config without `provider_balance_usd` fails closed and admits nothing. The dry run shows an effective recommendation of 0 for that case. This is an admission safety, not a spending reservation: with the file, `--sizing` lowers local admission from estimates (balance minus reported spend minus in-flight per-call caps) and is not aggregate enforcement.
- No sizing file means no cost estimate. Say "not estimated" instead of making up a number.
- Mock backends (`--backend mock`) cost nothing and show orchestration only.

## Step 2: Pre-launch statement

Before a paid run, retry, or revision, write down all of the following and show it to the user:

```
Backend/provider:        <haiku | named provider | mock>
Shape (A x D x S):       roots=<A> levels=<D> max_children=<S>
Maximum / intended tasks: <max_nodes> / <intended>
Plan limits:             spawn.max_children=<S> max_depth=<D-1> max_tasks=<n>
Concurrency (--workers): <n>
Per-call limit (--budget): <USD>
Aggregate cap authorized: <USD, from the user>
Retries / extra reviews: <counted separately, expected count>
Spend so far (reported): <USD or none>
Tokens so far:          <tokens or none>
Unknown-cost operations: <count or 0>
Source and plan checked: <plan path, hash or name, preflight result>
```

The plan's `max_tasks` and `max_depth` default to 128 and 4 (maximums 1024 and 16). If the intended shape needs more, the plan must set them explicitly. `irrigator plan PLAN` validates the plan first. Retries, replays, and additional review passes spend again. Count them in the aggregate and do not hide them inside the per-call figure.

## Step 3: Authorization gate

- Proceed with paid work only after the user gives an explicit amount and says to go ahead. "Run it" without an amount is not an aggregate cap. Ask. An authorization already given in this session (amount and scope) stays valid; do not re-ask for it.
- If the worst case (intended tasks × per-call limit, plus any retries) exceeds the authorized cap, say so and offer a smaller shape, lower concurrency, or the mock backend. Do not silently shrink the plan.
- If a run reaches the cap, stop with `irrigator pause DIR` (drain) or `irrigator down DIR` (cancel calls and retain progress), and report. Do not raise the cap yourself.
- An authorization covers the stated run, scope and cap, and persists for the session. A resume, replay-safe retry, build or export inside that scope and the remaining cap needs no fresh yes. A new scope, a higher cap, or a revision (`extra_cost_usd`) or retries that would exceed the cap needs a fresh statement and a fresh yes. An unresolved uncertain operation is never replayed on general authorization alone; it needs an explicit resolution or an explicit decision to accept duplication.
- If the user has not authorized spending, use mock work (`--backend mock`) or stop after the dry run. Do not call `apply`, `deliver`, `resume`, or `serve` on a paid backend.

## Step 4: Enforcement options (what actually exists)

| Mechanism | What it does | What it does not do |
|---|---|---|
| `--budget` on `apply`, `deliver`, `resume` | Per-call limit | Cap the whole graph |
| `--workers` | Hard ceiling on concurrency | Limit total tasks |
| Plan `max_tasks`, `max_depth`, `spawn.max_children` | Bound graph size | Bound cost |
| `apply/resume --sizing FILE` | Advisory opt-in local admission from operator-supplied estimates (balance minus reported spend minus in-flight per-call caps); saved as `RUN/sizing.json` and reused on resume | Reserve anything at the provider, enforce an aggregate cap, or make estimates accurate. Missing `provider_balance_usd` blocks new work |
| `serve --dir DIR --budget USD` | A shared local provider service reserves each call's per-call maximum against this allocation, refuses calls once spent-or-reserved plus the request exceeds it, and keeps reservations for pending and uncertain calls | Cover providers not connected through it, or bound an actual external charge that exceeds a reservation |
| `policy DIR FILE` | Bounds adaptive revisions (for example total extra cost) | Bound unreported charges or future calls |

For unmanaged plans, a configured provider service ledger enforces admission against its allocation, and only for calls that go through it. It bounds reservations, not the final invoice. The runtime tracks reported costs; interrupted calls can incur charges that were never reported. User monitoring is not enforcement.

## Step 5: Track actual spend

- `irrigator status DIR --json` and `irrigator decisions DIR` expose `reported_cost_usd`. Revisions archive superseded operations with their costs, so a revised run still counts earlier spend.
- For a pooled service, `ledger.json` and `operation-*.metrics.json` in the service directory record costs, sessions, and turns.
- Report the figure as "reported", compare it to the authorized cap, and call out retries separately.
- Do not infer a speedup, quality, or price-per-task from this data unless you also have a serial baseline measured on the same contracts.

## Guardrails

- The optional `irrigator mcp` `apply` tool (only with `--allow-control`) is not a budget exemption: it requires an explicit per-call `budget` and `backend` for non-managed plans, and managed plans use profile budgets. Container sandboxes and executors do not change spend.

- No made-up pricing, quotas, or provider balances.
- No public release, remote message, or push from this skill.
- Say what you did not check, for example "sizing file not supplied" or "authentication not verified (`doctor` shows CLI discovery only)".
