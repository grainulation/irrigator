# Child proposal normalization

Ownership-tree expansion reads the owner's spawn field through
[graph.go](../internal/graph/graph.go). [child_normalization.go](../internal/graph/child_normalization.go)
performs bounded, deterministic format normalization before the graph is decoded
and validated. It does not alter task meaning, retry a provider, or launch a call.
For main-agent turns and terminal helpers, use the separate
[turn contract](turn-shapes.md).

## Accepted forms

Both of these proposal shapes describe the same task list:

```json
{"tasks": [{"id": "review", "prompt": "Review the result", "requires": ["answer"]}]}
```

```json
[{"id": "review", "prompt": "Review the result", "requires": ["answer"]}]
```

The normalizer accepts one JSON array or object, rejects unknown top-level keys,
and requires each child to be an object. It rejects duplicate JSON keys,
case-colliding keys, trailing data, oversized input, and excessive nesting.
The source limit is 1 MiB; parsing is bounded to 32 nesting levels. Markdown
fences, prose extraction, and JSON-string wrappers are not accepted.

Allowlisted child display strings (`title`, `why_spawned`, `done_when`, and
`outcome_field`) may move from a child's top level into `display`. An existing
identical value is preserved; a conflicting value or wrong type rejects the
proposal. This normalization changes the derived decoder input only.

## Preserved authority and identity

The stored provider result is never rewritten. `ExpansionHash` hashes that raw
stored source, so formatting-equivalent proposals still retain their original
provenance. The graph's normal validation remains authoritative after normalization:

- Child count, depth, and total task limits.
- Unique namespaced IDs, ownership, dependencies, and cycle checks.
- Effective provider, allowed context, local output paths, and validator restrictions.
- Child effort at or below the owner's effort, or inherited when omitted.
- Parent acceptance contracts and all-or-nothing admission.

A rejection admits no children. Normalization does not reconcile uncertainty or
make an uncertain owner eligible for expansion.

## Diagnose a rejected proposal

Start with read-only evidence:

```sh
irrigator status /absolute/run --json
irrigator inspect /absolute/run OWNER
```

Distinguish a format error from a provider, authority, or acceptance error. Keep
the original output and operation identity available for diagnosis. Do not edit
managed checkpoints or silently rewrite retained provider results.

`resolve DIR NODE --result FILE` is the supported reconciliation path for an
uncertain operation; it requires the relevant controller/process conditions to
be satisfied. Use `--confirm-stopped` only after independently confirming that
surviving provider processes have stopped. `retry` starts a new operation and may
repeat external effects or incur new charges. See [recovery](recovery.md) and
[debug workflow](debug-workflow.md) before choosing an action. There is no
`revalidate` command for repairing a failed retained artifact in place.

Regression coverage is in `child_normalization_test.go`
(next to the code in [internal/graph](../internal/graph)) and `child_normalization_author_test.go`
(in [internal/runops](../internal/runops)),
including raw-hash preservation, unsafe proposals, atomic rejection, and
uncertain-owner handling.
