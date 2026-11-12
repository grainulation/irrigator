# Experiments

Experiments are opt-in features outside the stable contract. They are off unless
a plan declares an `experiments` block; a plan without one keeps its exact bytes
and plan hash. `irrigator experiments` lists what is available and its risks.

## Messaging

Verified outputs travel as **notes**: between tasks of one run (edges) and,
within one campaign, between runs (channels). Nothing reaches an attempt that is
already running.

```json
"experiments": {
  "messaging": {
    "enabled": true,
    "edges": [{"from": "research", "to": "write", "field": "finding"}],
    "publish": [{"channel": "api-spec", "task": "research", "field": "finding"}],
    "subscribe": [{"channel": "api-spec", "task": "write"}],
    "max_bytes": 4096, "max_per_task": 8, "max_per_run": 256, "max_hops": 2,
    "cross_swarm_gate": "review"
  }
}
```

| Rule | Why |
| --- | --- |
| Only declared edges; they must form an acyclic graph no longer than `max_hops` (default 2, at most 4). | No loops or message storms. |
| The note is a declared output field of the source and travels only once that task is **verified**. | One agent's mistake does not cascade unchecked. |
| Notes are collected when an attempt **begins** and frozen on the task row (`notes`); a recovered attempt reuses them. A `notes_delivered` event records sources and digests. | Deterministic resume; no mid-attempt injection. |
| Notes reach the model as quoted data with provenance under the reserved context key `@notes`, and grant no tools, budget or permission. | Injection is framed as data and cannot escalate privileges. |
| Size, per-task and per-run limits (defaults 4096 bytes, 8, 256; maxima 16384, 32, 1024). Longer notes are truncated and marked. | Bounded fan-out and cost. |
| Channels connect only runs admitted to the **same campaign**, never deliver a run's own posts, and never admit work by themselves. | Cross-run messages stay inside one shared budget. |
| With `cross_swarm_gate: "review"`, published items wait as `pending` until approved. Tampered channel files are ignored. | A human can gate what crosses between swarms. |

Edges do not make a task wait: a target receives whatever verified notes exist
when its attempt begins. Use `depends_on` when the order matters.

### Channels

Channels live in the campaign directory (`CAMPAIGN/channels/NAME/ITEM.json`). The
controller publishes each verified publishing task once, and repairs a missed
publication on its next pass.

Declare at most one published field for each `(channel, task)` pair. New plans
with different fields for the same pair are rejected before run creation or
campaign reservation. Repeating the same field is harmless and remains idempotent.
Use distinct channels or combine the fields into one declared output when both
must travel. Admitted plans remain readable; an ambiguous publication stops with
an explicit conflict instead of discarding an output. Existing publications are preserved, and a publication pass
accepts an existing item only when its provenance and text match the verified
output. Approval and the original publication time are retained.

Channel plans are loaded through `campaign admit`, which validates their tasks,
fields and execution limits before allocating a grant, then assigns and validates
the real campaign binding. Standalone `plan`, `prepare` and `apply` continue to
reject unbound channel plans. Admission leaves the input plan file unchanged;
the admitted run pins its campaign-bound plan and hash.

```sh
irrigator campaign init ./campaign --budget 10
irrigator campaign admit ./campaign plan-a.json --run ./runs/a
irrigator campaign admit ./campaign plan-b.json --run ./runs/b
irrigator channel list ./campaign api-spec
irrigator channel approve ./campaign api-spec ITEM   # when cross_swarm_gate is review
```

Pending items are listed and approved with `channel list` and `channel approve`.

### Not included

Messages into running attempts, agent-chosen recipients, network transport (an
A2A adapter could come later), and messaging between host agent sessions.
