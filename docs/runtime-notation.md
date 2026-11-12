# Irrigator runtime notation

This page is the plugin-facing reference for the notation the Irrigator CLI actually understands. It describes runtime behavior of the local Go executable (`irrigator`, version 1.0.0-rc.4). The executable also provides a stdio MCP server (`irrigator mcp`, read-only unless started with `--allow-control`; see [mcp.md](mcp.md)). This page does not describe a hosted service, pricing, or a public release; none exists for this plugin.

The wire/compatibility names `swarm/v1`, `swarm.worker/v1`, `SWARM_*` and the saved-run filenames (for example `state.json`) are stable contracts, not stale product names. The product and executable are Irrigator.

Related references cover: swarm shapes, shape sizing, measured sizing, observer shape contract, public display metadata, live session and session registry v2, plus the README and AGENTS.md. If this page and the executable disagree, the executable (`irrigator COMMAND --help`) wins.

## 1. Shape notation: A×D×S

`A×D×S` (ASCII `AxDxS` is equivalent) describes a bounded recursive swarm.

| Symbol | Meaning |
| --- | --- |
| A | root agents started by the coordinator |
| D | TOTAL agent levels, including the roots as level one (at most 16) |
| S | maximum children each node may spawn |

- The coordinating agent is outside the count.
- A, D and S are positive integers.
- Two-dimensional `W×D` is shorthand for `W×D×1`: `2x5` is two chains of five, ten agents.
- The maximum node count is `A × (1 + S + S² + … + S^(D−1))`. When S=1 this is `A×D`.
- The runtime shape bound is 1024 nodes, checked at each step so it cannot overflow.
- The maximum is an upper bound, not a promise. A limit of S permits fewer children. Always say whether the run targets the fully populated tree or a smaller realized graph.

| Shape | Roots | Levels | Max children | Maximum agents |
| --- | ---: | ---: | ---: | ---: |
| 1x1x1 | 1 | 1 | 1 | 1 |
| 2x5 (2×5×1) | 2 | 5 | 1 | 10 |
| 5x6 (5×6×1) | 5 | 6 | 1 | 30 |
| 10x2 (10×2×1) | 10 | 2 | 1 | 20 |
| 10x2x9 | 10 | 2 | 9 | 100 |
| 3x3x3 | 3 | 3 | 3 | 39 |
| **25x5x2** | 25 | 5 | 2 | **775** |

A 25x5x2 tree has 25, 50, 100, 200 and 400 agents at successive levels: **775 total, not 125**. A shape whose maximum exceeds 1024 (for example 2x10x2 = 2046) is outside the runtime bound.

## 2. Shape is not concurrency

Root count does not set concurrency. Choose `--workers` separately, from provider limits, latency, resources and spending cap. Waiting ancestors do not occupy worker capacity. Twenty-five roots do not imply 25 workers, and a 775-node maximum does not imply 775 simultaneous calls.

`--workers` (default 2) is a hard ceiling. Opt-in sizing can only lower admission below it; it never raises it.

Ownership depth D is not the dependency critical path. Actual dependencies and runtime expansion are unknown to a shape dry run, so it gives no exact makespan.

## 3. Expanding a shape into a plan

A shape is shorthand. `apply` still needs an explicit plan (`api_version: "swarm/v1"`, a name, a `tasks` array). Expand as follows:

| Shape element | Plan expression |
| --- | --- |
| A roots | A root tasks with no parent |
| S | `spawn: {"field":"children","max_children":S}` on every task allowed to spawn |
| D levels | plan `max_depth` = D−1 (plan roots have depth 0; default 4, maximum 16) |
| Maximum nodes | plan `max_tasks` large enough for the intended tree (default 128, maximum 1024) |
| Leaves | tasks at the last level carry no `spawn` and must not spawn |

For a full 25x5x2 tree: 25 roots, `max_children` 2, `max_depth` 4, `max_tasks` 775. The default `max_tasks` of 128 is far too small, so set it explicitly.

Spawning details:

- A spawning task's required output field (for example `children`) holds a JSON string containing a `tasks` array.
- Child IDs are scoped as `parent__child`. Local dependency IDs refer to siblings.
- The parent waits until its children are verified before its dependents run.
- Children inherit the parent's provider and effort. They cannot broaden context or introduce command validators.
- Expansion is committed as one state transition. Recovery reuses committed children instead of duplicating them.
- A static list of tasks does not demonstrate agent spawning. To test recursive delegation, require the agents to return real child manifests.
- If `max_tasks` or the spending cap stops expansion, report the smaller realized graph honestly. Never describe it as a completed full tree.

### Optional `observer_shape`

A plan may carry read-only metadata in the same notation:

```json
"observer_shape": {"roots": 25, "levels": 5, "max_children": 2}
```

- It grants no capability: no spawn, depth, task, provider or context rights.
- It is checked only when a plan is first loaded or a run is initialized, not during later graph revisions.
- `roots` must equal the plan's root task count. `levels−1` must not exceed the effective `max_depth`. When levels>1, every root needs a spawn policy whose effective cap is at most `max_children`.
- Read-only projections compare it to the active graph. On any mismatch they report `known=false` with a reason and no ceilings; the shape stays as provenance only.
- Omitting it leaves the plan hash unchanged for legacy plans.

## 4. Dry-run sizing

```sh
irrigator size --shape 25x5x2 --sizing examples/sizing-shape.json --budget .25 --backend haiku [--json]
irrigator size PLAN --sizing FILE [--budget USD --backend NAME] [--json]
```

- The command makes no provider calls, runs no work and mutates no plan or run. PLAN and `--shape` are mutually exclusive (exit 2).
- It reports shape, `max_nodes`, levels, roots, max children, a cost range from roots-only to full expansion, predicted and recommended workers, and uncertainty.
- `feasible` covers admitting the roots only. Inspect `full_expansion_within_budget` and `full_expansion_meets_reliability_floor` separately in the JSON.
- Estimates are `measured` or `assumed`. The shipped example is entirely assumed. `modelled_frontier_upper_bound_s` is a scenario bound, not a critical-path prediction.
- The shape dry run never authorizes spending.

## 5. Spending: what is and is not a cap

- `--call-budget` (or the `--budget` alias) on `apply`, `resume` and `deliver` is a PER-CALL limit in USD (default 0.25). It is not a graph-wide cap.
- Per-call limits alone do not enforce a graph-wide budget.
- Opt-in runtime sizing (`apply PLAN --run DIR --sizing FILE`, saved to `RUN/sizing.json`, reused by `resume`) adds a per-provider reservation bound. It requires `provider_balance_usd`, which is an operator-supplied allocation estimate, not a fetched balance. If the configuration is missing, the policy fails closed and admits nothing. The provider's durable ledger remains authoritative.
- Revision policy defaults (a different mechanism) cap estimated extra cost at $0.25 and reported total plus extra at $5, but cannot bound unreported charges or future calls.
- Interrupted calls can incur charges missing from reported totals.
- Before any paid run, state: roots, levels, max children, maximum and intended node counts, workers, per-call limit, and the aggregate cap that is actually enforced (and by what mechanism). A user-supplied figure such as "$9.50" is a launch-time agreement, not a runtime guarantee, unless an enforcing mechanism is configured.
- Afterwards report actual nodes and model calls, retries, observed levels and branching, completion and reported cost. Reviews and repair calls count separately and do not change the declared tree.
- Prefer `--backend mock` for routine checks: it needs no account, network or spend, and demonstrates orchestration, not model quality or speed.


## 6. Public run and task metadata

Plans may carry optional, producer-authored display strings. They are not verified, and execution status is reported separately.

Plan `display`: `{version:1, publish, title<=60, purpose<=140}`. Any other version is rejected.

Task `display`: `{title<=48, why_spawned<=100, done_when<=140, outcome_field}`. Limits count runes. A non-empty `outcome_field` must name a field in the task's `requires` and must not be a `file_fields` entry.

No shipped viewer renders these strings; the fields stay valid plan schema for compatibility. The metadata carries no ETA, baseline or worker-capacity fields.

Spawned children may carry the same task display fields. See `examples/display-plan.json` in the Irrigator checkout.

## 7. Run lifecycle commands (as in the CLI usage text)

```text
init --out plan.json            plan PLAN [--json]              prepare PLAN --run DIR
apply PLAN --run DIR [--workers N] [--budget USD] --backend haiku|mock|NAME [--sizing FILE]
deliver PLAN --run DIR --dir NEW_DIR --nodes IDS [--check JSON_ARGV] [--recheck]
resume DIR [--retry-failed] [--workers N] [--sizing FILE]
status DIR [--json] [--watch]   inspect DIR NODE     graph DIR     logs DIR [--follow] [--since N] [--task ID] [--json]
pause DIR | down DIR            cancel DIR NODE         retry DIR NODE [--recursive] [--allow-uncertain]
resolve DIR NODE --result FILE [--confirm-stopped]
propose DIR FILE | decisions DIR | policy DIR FILE
export DIR [--out FILE | --dir NEW_DIR]   gc DIR [--apply] [--keep N]
review tui    session list|show    mcp    serve --dir DIR    connect FILE    doctor    version    demo [--run DIR]
```

Rules that skills must preserve:

- **Preflight the exact source.** Validate with `plan PLAN` and read the exact files named before paying; `doctor` distinguishes CLI discovery from authentication and never claims that finding an executable verifies login.
- **Immutable plan.** The original plan and hash never change. Unfinished work is changed only through `propose` (complete next active task list plus `base_revision`). Verified results keep their operation identities and are not replayed. Goals and acceptance checks cannot be removed to manufacture success.
- **No blind uncertain replay.** A manual provider's abandoned call becomes `uncertain` and blocks automatic replay. Inspect the external system, then either `resolve DIR NODE --result FILE` (add `--confirm-stopped` only after personally confirming surviving processes are stopped) or, with explicit user acceptance, `retry DIR NODE --allow-uncertain`. `resume --retry-failed` retries replay-safe failures only.
- **Existing run directories are never overwritten**; `export --dir` needs a destination that does not exist.
- **Pause vs down.** `pause` drains calls and stops scheduling; `down` cancels calls and keeps progress. Wait for the controller to exit before resuming.
- **Acceptance is independent.** Only `verified` results release dependents. `required` fields and `checks` are structural; `go_source` checks syntax only. Compilation and behavior need a command validator or a separate integration gate. A successful model call is not acceptance.
- **Dispatch evidence.** Cite `status --json`, `inspect`, `logs --json`, and the events/costs actually recorded, not the chat narrative.

## 8. Observation (secondary)

Observe in the terminal: `irrigator status DIR --watch`, `graph DIR`, `logs DIR`, `inspect DIR NODE`. They create no tasks and make no model calls.

Several runs can share one registry:

```sh
irrigator apply PLAN --run DIR --session-registry /abs/registry.json
```

Registry v2 keeps up to 16 recent and 32 active entries (64 per file, at most 64 KiB), never overwrites another run, and migrates v1 files on the next actual start. A start is rejected when 32 OTHER active runs already exist. Status is derived from durable state and locks, never chat: `complete` needs `control=complete`, every node verified and no live lease; otherwise `inconsistent`. Others are `running`, `stalled`, `paused`, `blocked`, `ready`, `indeterminate`, `missing`, `unavailable`.

An event tail is a bounded window. It is not complete history, `seq` gaps are normal, and spawn/handoff have no emitter. Observation is not a source of acceptance.

## 9. Known non-features

No hosted control plane (the MCP server is local stdio only), remote provisioning, learned model router, or general code-editing tool exists. Cells are local directories and trusted processes, not sandboxes. The store targets bounded single-host graphs on a local filesystem. Mock scaling does not establish live-model speedup.
