# Routing and checking local work

`profiles route` applies explicit role/difficulty choices to an existing managed
plan. It uses profiles already present in that plan's catalog and allowlist.
It previews the selected model and per-operation budget for every task; without
`--out` it writes nothing. With `--out`, it creates a new validated plan and
refuses to overwrite a file. It never edits an admitted run or retries work.

```sh
irrigator profiles route examples/managed-profiles-v3.json --policy examples/routing/role-policy.json
irrigator profiles route examples/managed-profiles-v3.json --policy examples/routing/role-policy.json --out routed-plan.json
```

The example uses mock profiles. For real work, choose research, implementation
and review profiles in your catalog, then reference their names in the policy.
`easy`, `medium` and `hard` are explicit operator hints; the runtime does not
guess task difficulty. Tasks absent from `tasks` keep their previous profile.
An explicit hint replaces that task's previous profile in the **new** plan;
the preview includes both names. Helpers and continuations still follow existing
profile-inheritance rules. A requested model name does not prove availability or
actual model identity. Budgets cap admission/reservations; they are not invoices
or guarantees that a provider cannot overrun.

## Provider limit advice

```sh
irrigator quota runs/first draft --attempt 1
```

This read-only command inspects that exact attempt's captured stdout/stderr and
returns recognized rate-limit/reset information. It does not print provider
prose or credentials. Relative retry durations are estimated from the captured
file's modification time; ISO timestamps and epoch reset fields are supported.
Conflicting estimates leave `reset_at` null. Missing or unreadable evidence is
reported, and an elapsed estimate remains an advisory rather than proof that an
account is ready. Session/operation budget failures remain distinct from account
quota. No result permits automatic retry, fallback or replay of uncertain work.
Historical attempts do not inherit the current attempt's binding or diagnostic.

## Compact delivery evidence

```sh
irrigator delivery-summary runs/first
```

The report joins the recorded delivery selection with current task outcomes:
included/excluded nodes, declared check counts, node-contract verdicts, validation
level, file conflicts, graph revision and a deterministic candidate digest.
Each declared check has an outcome and its evidence basis. Accepted node contracts
establish passed checks; a recorded first check failure establishes the preceding
passes and following checks as not run. Interrupted checks stay unknown. Historical
states without individual receipts are not presented as new executed observations.
It reports file/directory collisions and disagreements in selected output maps.
No source content is printed. Candidate identity requires valid selected maps
without conflicts or stale provenance. A node-contract pass is distinct from an
acceptance command: `acceptance` is derived from the existing delivery journal
and stays `not_configured` when no acceptance command ran. Failed or interrupted
acceptance stays failed/unknown; viewing a report never reruns a check or changes
the journal. The report identifies a candidate; it does not apply or approve it.

## Credential-free comparison harness

```sh
irrigator benchmark --dir ./benchmark-run --runs 2
```

The harness creates a fresh private directory and runs the same fixed fixture
through the real mock runtime with one and two workers. Repeated runs alternate
arm order. An independent grader checks held-out square values and dependency
completion after each arm; a verified worker status alone cannot satisfy it.
`benchmark.json` retains plan hashes, correctness, wall time, reported costs and
unknown-cost counts. It returns a nonzero exit when an arm fails grading.

Mock costs are zero. This measures scheduler overhead and fixture correctness,
not model quality, provider token economics or a general speed/cost saving.
There are no credentials, network requests or live model calls. A live comparison
requires a separately authorized budget and matched held-out tasks.

For explicit 900/2,000/10,000-task workloads, restart and backup checks, and
separately authorized local-model generations, use the [scale harness](scale-checks.md).
The original five-task comparison remains credential-free and mock-only.
