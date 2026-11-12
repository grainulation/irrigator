# Main agents, turns, and terminal helpers

The turn-aware convention keeps three separate quantities:

- **X** independent main-agent identities.
- **Y** optional sequential turns per main agent, including the initial turn.
- **Z** on-demand, asynchronous one-shot helpers per main-agent turn.

Shorthand reserves **Z=1** for an unbranched view, preserving `111` as one
execution. The canonical helper allowance **H** is zero in that case and Z
otherwise. Explicit `Z=0` also means no helpers. Canonical fields use actual
counts: `helpers_per_turn: 1` means one helper, not zero.

The CLI requires separators, for example `2x2x2`; compact `222` is conversational
shorthand only and is not parsed because multi-digit values would be ambiguous.

Each main-agent lane progresses independently. Agent A can reach turn 2 while B
is still on turn 1. Turn 4 of A requires A's turn 3 to complete. Early stopping
does not create failed or pending tasks for unused turns. Each `(agent, turn)`
pair has its own cumulative helper allowance; completed helpers do not refill it.
Helpers have one execution and cannot take continuation turns or spawn children.
Retries are attempts of the same execution, separately budgeted.

| Shape | Main agents | Optional turns per agent | Helpers per turn | Maximum executions | Maximum distinct agents |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1×1×1 | 1 | 1 | 0 | 1 | 1 |
| 1×1×2 | 1 | 1 | 2 | 3 | 3 |
| 2×2×1 | 2 | 2 | 0 | 4 | 2 |
| 2×2×2 | 2 | 2 | 2 | 12 | 10 |

Execution capacity is `XY(1+H)`. Distinct-agent capacity is `X+XYH`. These are
ceilings, not promised expansion or simultaneous workers. In particular 222 is
two agents with two independent turn sequences, not four main agents.

## Executable versioned plans

`swarm/v2` plans execute turn-aware workflows through the durable scheduler, while
`swarm/v1` plans follow ownership trees. The credential-free
`examples/turns-v2.json` executes two stable main agents, two turns each and two
helpers per turn: **12 executions and 10 agent identities**.

```sh
irrigator apply examples/turns-v2.json --run /absolute/workspace/turn-run --backend mock --workers 4 --session-registry /absolute/workspace/.irrigator/current-session.json
```

A v2 plan supplies canonical `turn_shape` (`root_agents`, `max_turns_per_agent`,
`helpers_per_turn`) and `max_tasks` equal to its execution ceiling. Only initial
main tasks are user-admitted. Each carries `turn: {agent_id, role: "main", index: 1}`.
Canonical helper count 1 really permits one helper; the CLI shorthand sentinel
is not applied to stored plans.

A main task may return a JSON object encoded in its `turn_request` result field:

```json
{"continue":{"prompt":"Continue using accepted results","requires":["answer"]},"helpers":[{"id":"research","prompt":"Perform one bounded task","requires":["answer"]}],"wait_for_helpers":true}
```

If `turn_request` is declared in `requires`, return `""` or `"{}"` to stop that
lane early; the declared key must still be present. A task that does not declare
the field can omit it. Other required outputs must remain nonempty. The
runtime assigns continuation and helper IDs and metadata. Generated requests may
not supply ownership, turn metadata, recursive child spawning, arbitrary dependencies,
new executable validators, expanded context, or a different provider. Each turn
admits its entire bounded request atomically with verification of that turn.

A `turn_request` admits helpers **at turn boundaries**. The local `helpers`
command also admits helpers while a main provider call is running, as described
below. The next main turn depends on its accepted predecessor. With
`wait_for_helpers: true`, it also depends on and receives the results of that
turn's helpers; otherwise helpers and continuation can proceed concurrently.
Unverified helper results are never inherited implicitly. Each agent progresses
independently, subject to the existing worker limits and provider admission.
Stable agent identity and explicit accepted predecessor results are persisted in
the checkpoint. The `serve` adapter additionally persists explicit prompt/output
history, with the scope and acceptance rules below.

Helpers cannot continue or spawn. A duplicate finalization cannot refill helper
allowance or admit another continuation. Existing operation IDs, attempt leases,
uncertain-outcome reconciliation and atomic checkpoint replacement still govern
execution. Recovery retains uncertain external calls instead of automatically
replaying them. V2 graph revisions can improve admitted, unstarted work while
preserving lane identity and topology, as described below.

Snapshot nodes and debug tasks expose optional `turn` identities; snapshots
include `execution_model: "turns/v1"` and the canonical shape.

`irrigator turn-shape 2x2x2 --json` previews capacity; it does not
admit or execute a plan. `size --shape` evaluates A×D×S ownership-tree plans.

## Helpers during a running call

A trusted local workflow or process provider can submit a bounded request while
its main turn is running:

```sh
irrigator helpers /absolute/run main-task-id --operation EXACT_OPERATION_ID --attempt 1 --request helpers.json
```

```json
{"id":"research-batch-1","helpers":[{"id":"research","prompt":"Answer one bounded research question","requires":["answer"]}]}
```

Read the current operation and attempt from `inspect`/`status --json` or the
provider request. They fence the exact live owner attempt; they are not a secret
or authentication token. Access to the run directory grants local mutation
permission. Admission requires a running, uncancelled main turn and running
controller, and commits the whole batch atomically. Identical batch-ID retries
return the existing admission, including after the owner settles. Reusing a
batch ID with different content is rejected.

The controller schedules these helpers under its existing worker limit. With
spare worker capacity they can finish before the main provider call returns.
They receive the owner's already verified dependencies and permitted selected
context; they never receive unfinished owner output. Their provider, effort,
validators, output paths and context remain bounded by the owner. Helpers remain
terminal: they cannot spawn or continue. Mid-call and boundary helpers share the
same cumulative per-turn allowance. Finishing a helper does not refill it.

A main turn can fail after its asynchronous helpers have completed. Explicit
node retry and `resume --retry-failed` rerun eligible failed main work as a fresh
operation; helper completion never substitutes for the main's own acceptance.
Previously verified helpers, original admission receipts and their spent
allowance remain unchanged. Those grants stay tied to their original operation
in the same owner's lineage; new admission requires the current operation and
attempt. Nonrecursive owner retry does not reactivate cancelled helpers.
Uncertain work still requires reconciliation or a separately authorized explicit
uncertain retry; `--retry-failed` never replays it automatically.

A final `turn_request` with `wait_for_helpers: true` joins both already-admitted
mid-call helpers and newly requested boundary helpers before the continuation.
Without that option, continuation and helpers may proceed independently. There
is no streaming result injection into an already-running model call: a local
host can inspect a verified helper result, or the next turn can consume it.
The built-in Claude adapter has tools disabled and cannot itself invoke this
command; a trusted host workflow/process provider must do so. Admission does not
raise a worker limit, helper allowance or paid-call budget.

## Turn-aware revisions

The `propose` transaction supports v2 improvements to pending,
never-attempted admitted tasks. Include the complete current task list and its
current graph revision. Task IDs, turn identities, predecessor/dependency edges,
input-field selections and ownership must stay unchanged. Provider/context and
acceptance requirements cannot expand or weaken. The initial plan and hash stay
immutable; accepted revisions archive replaced generations and fence their
operation IDs, just as v1 revisions do.

Running, produced, verified, failed, cancelled, uncertain or previously attempted
executions cannot be rewritten by this v2 revision mode. Asynchronously admitted
helper specifications are also immutable. Revisions cannot add/remove lanes,
insert turns, reshape helper ownership or change capacity; those require a new
plan or the existing bounded turn/helper admission flow. Proposal replay remains
idempotent and unresolved external effects remain protected.

## Durable explicit conversation history

For v2 main turns sent through `serve`/`connect`, the provider ledger stores the
explicit task prompt and structured output alongside each operation receipt.
The next turn inherits a predecessor's history only when run ID, agent ID,
predecessor task, sequential turn index and selected-context hash match, and the
predecessor's completed successful output exactly matches the accepted dependency
result supplied by the runtime. Pending, uncertain or failed outputs are never
added to inherited history. Helpers and other agents do not inherit it.

A fresh CLI process reconstructs this explicit history after service restart or
worker rotation. History is data in the next request, not a replay of earlier
operations. Cached operation receipts remain idempotent. This preserves
per-operation reservations and avoids implicit cross-lane CLI session context.
It is not vendor-native conversation resumption or persistence of hidden model
reasoning, tools, or transcript internals.

History is limited to 64 explicit entries and 1 MiB, and the complete current
request plus history must fit the 1 MiB request bound. Oversized input history is
rejected before provider dispatch. A result too large for history still retains
its durable operation result, but does not retain a conversation snapshot.
Changing selected context or using a predecessor with no matching successful
receipt starts a new explicit history; accepted dependencies still arrive in the
normal request. Session behavior and operation receipts are preserved.

### Worker boundary guidance

Built-in managed model requests include an `execution_boundaries` snapshot. It
names the topology (`turns`), current turn, remaining main turns, available helper
slots, remaining run task slots, and whether waiting for helpers is possible.
The snapshot is guidance rather than a capacity reservation; admission rechecks
all limits when the result returns.

The model-facing `turn_request` schema requires a JSON object: use `{}` to stop,
or an object with bounded helpers and an optional continuation. An empty
string is also accepted as a stop value by the runtime and stored protocol. Helpers must include
`id`, `prompt`, and nonempty `requires`. A final turn cannot request a continuation
or `wait_for_helpers: true`; a helper cannot create work. An earlier main turn can
wait only with a continuation. Object results normalize to the existing JSON
string in checkpoints and worker results, preserving the stored protocol.

For new plans, statically known invalid helper waits in `mock_result` (including
nested continuations) fail initial validation before creating a run. Old admitted
checkpoints keep their original validation contract. Model-generated requests
still undergo runtime validation because their contents are unknown beforehand.
