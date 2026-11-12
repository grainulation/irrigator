# Measured swarm sizing (opt-in)

Sizing chooses how many workers run at once. It is off by default. Without `--sizing` and without `RUN/sizing.json`, admission, the plan hash and defaults are unchanged.

There are two dry-run entry points: `irrigator size PLAN` models an existing plan, and `irrigator size --shape A×D×S` gives shorthand guidance before a plan exists (see [Shape sizing](#shape-sizing-axdxs)).

```
irrigator size PLAN --sizing examples/sizing.json [--budget 0.25 --backend haiku] [--json]   # dry run, no provider calls, writes nothing
irrigator apply PLAN --run DIR --sizing FILE      # enforce at admission
irrigator resume DIR [--sizing FILE]              # reuses DIR/sizing.json if present
```

`--sizing` copies the file to `RUN/sizing.json`, outside the hashed plan. Delete it to disable sizing. `--workers` stays a hard ceiling. Sizing only lowers admission.

## Model (seconds, USD)

Per remaining task `i`:

- `d_i = (s_i + k_i*o_call + t_i*o_tool + r) * (1 + q_i)`. `r` (`overhead.residual_s`) is counted once per task. `q` is the operator risk input `retry_p`; no retry is ever added.
- `d+_i = d_i + z*sqrt(var_i)*(1+q_i)`, where `z` is `risk_z` (default 1).
- Expected cost `C = sum c_i*(1+q_i)`. Budget estimate `B = budget_usd - reported spend`.
- Reliability `R(N) = prod(1 - min(.99, q_i*(1+gamma*(N-1))))`.

Precedence edges are dependencies and ownership (child to parent; a `waiting` owner is a zero-duration barrier). Tasks sharing an output path are **mutually exclusive** in the scheduler, not chained, so conflicts cannot create cycles. `L_conf` is the largest per-path sum of `d`.

`T(N)` and `T+(N)` are deterministic list schedules (longest remaining path first) on N slots, from 1 to min(`capacity`, `--workers`, remaining tasks). **Selection uses the conservative `T+`**: among N with `R >= reliability_floor` (default 0.9), take the smallest N with `T+(N) <= (1+eps)*min T+` (`eps` default 0.02).

## Reservation bound (runtime, per provider)

The opt-in runtime policy requires `provider_balance_usd`: provider name to the configured starting allocation for this run. These values are supplied by the operator, not fetched account balances. Missing configuration or a missing provider entry fails closed. At each admission, per provider:

`new_slots = floor((balance - recorded spend of that provider's active and archived tasks - busy tasks of that provider * per_call_budget) / per_call_budget)`

- `per_call_budget` is the runtime `--budget` (`opts.Budget`). Configuration cannot understate it, and `cost_usd` is not used.
- A task's provider is its `provider`, else the run backend. Each busy task is one reservation, subtracted once.
- Per-provider recorded spend includes archived tasks. The provider of an archived task is `archived.Spec.Provider`, falling back to `State.Backend`. The admission cache key includes the archived provider and cost.
- A task is admitted only if its provider has an entry in `provider_balance_usd` and `new_slots > 0`. Otherwise it is **not admitted** (fail closed).
- Example: balance 0.30 and cap 0.25 gives 1 slot. With one busy, 0 new. Once spent, 0.
- The dry run reports `reservation_slots_estimate` and `reservation_limited_n` from the `--budget` flag. They are estimates; **the provider ledger remains authoritative** and is never read here.

## Safety and fallbacks

Budget and reliability are checked before any fallback.

| Case | Result |
|---|---|
| Expected cost > budget estimate | `feasible=false`, `chosen_n=0`, shortfall reported |
| Reliability < floor even at N=1 | `feasible=false`, `chosen_n=0` |
| No service-time data, safety passes | N=1, no speed claim |
| More than 1024 remaining tasks | refused (`chosen_n=0`); unmodelled |
| Dependency cycle | N=1 conservative (safety already passed) |
| Reservation capacity 0 | no task admitted |

When the gate admits nothing, it returns an empty ready list. The controller then moves to `blocked` if no worker is busy. If workers are busy, the controller keeps waiting as normal.

The admission cache is keyed by a fingerprint of graph revision, per-task status, parent, dependencies, outputs and recorded cost, including archived provider costs, plus worker limit and reservation slots. A `waiting` barrier therefore differs from `running`.

## Inputs and provenance

Every estimate is `measured` or `assumed`. Measured inputs require `calibration.source` and `calibration.n_samples`, which must reflect real samples. A measured `residual_s` combined with `per_call_s`/`per_tool_s` is rejected. The runtime reads only this aggregated file. The shipped example is entirely `assumed`, with no calibration.

## Shape sizing (AxDxS)

Shape sizing (A×D×S) is shorthand planning for a swarm that does not have a plan yet.

```
irrigator size --shape 25x5x2 --sizing examples/sizing-shape.json --budget .25 --backend haiku [--json]
```

No PLAN is needed; `size PLAN` still works. PLAN and `--shape` are mutually exclusive (exit 2). The command never runs work, makes provider calls or mutates a plan.

- `A` = root agents, `D` = TOTAL ownership levels including roots (at most 16), `S` = maximum children per node. ASCII `x` or `×`. `W×D` means `W×D×1`.
- The coordinator is excluded. Workers/concurrency are separate from nodes.
- `max_nodes = A * sum(S^k, k=0..D-1)`; with S=1 this is `A*D`. Examples: 1x1x1 = 1, 2x5 = 10, 25x5x2 = 775.
- Components must be positive integers. The bound is 1024 nodes, checked at every step so it cannot overflow. A single component above 1024 (for example `1x1x1025`, reported as max children exceeding 1024) is also rejected.
- The maximum is an upper bound, not a promised expansion.

Output: shape, max_nodes, levels, roots, max_children, a cost range from roots-only to full expansion (from calibrated `defaults`, retry applied), predicted workers (modelled, unconstrained), recommended workers (limited by reservation slots for `--backend`, with `--budget` as the per-call cap), and uncertainty.

Infeasible budget or reliability is reported with `feasible=false`. Feasibility is judged on roots-only. Separate flags say whether full expansion fits the budget and the reliability floor.

Ownership depth is NOT the dependency critical path. Actual dependencies and expansion are unknown here, so no exact makespan is given. `modelled_frontier_upper_bound_s` is a full-expansion scenario bound at `predicted_workers`, assuming identical durations and no additional dependency or file-conflict edges. Reservation-limited execution can be slower. The runtime recomputes sizing from the materialized DAG.

In shape mode, `feasible` covers admitting the roots under the configured budget, reliability and reservation bounds. It does not promise that the maximum tree can finish: inspect `full_expansion_within_budget` and `full_expansion_meets_reliability_floor` in JSON separately. (In plan mode, `feasible` follows the table in [Safety and fallbacks](#safety-and-fallbacks).)

Runtime safety for shape-derived configs is the [reservation bound](#reservation-bound-runtime-per-provider): at runtime, a sizing config without `provider_balance_usd` fails closed and admits nothing, and the runtime `--budget` per-call cap is authoritative for reservations. The dry run shows the unconstrained model with a warning and an effective recommendation of 0. Provider allocations are operator-supplied estimates, not balances fetched from a service. The durable provider ledger remains authoritative.

## Limits

- Residual is elapsed minus API time; it is not isolated network or tool time.
- Completed-only telemetry is selection-biased and has no failures. `retry_p`, calls and variance are operator inputs.
- Budget and reservation figures are estimates. They are not provider balances.
- `ready_width_diagnostic` is not a cap.
- The result is the minimum over evaluated candidates for this model, not a global optimum or speedup claim. Dry run cannot see runtime-spawned tasks.
- Running tasks are modelled at full duration. A busy task's earlier recorded spend is counted in addition to its reservation, which is conservative.

Shape timing (see [Shape sizing](#shape-sizing-axdxs)) is a scenario bound for a full ownership tree with identical durations and no additional dependency or file-conflict edges; it is not an actual critical-path prediction.
