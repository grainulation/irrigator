# MCP lifecycle controls and completed deliveries

Use `--profile execution --allow-control` for run control, and
`--profile delivery --allow-control` for optional candidate assembly and checking.
Tool inventories stay fixed for each server session. The default legacy profile
retains its existing inventory and behavior.

## Cancelling a cell or stopping a run

Read `run_status` first. Its `run_id`, `plan_sha256` and `graph_revision` identify
the saved graph. For cancellation, also read `inspect_cell` and retain the
returned `node.operation_id`. Canonical naming changes `run_status` to
`get_run_status`; the identity fields are the same.

`cancel_cell` requires:

```json
{
  "run": "/workspace/runs/example",
  "request_id": "cancel-review-1",
  "expected_run_id": "value from run status",
  "expected_plan_sha256": "64 hexadecimal characters from run status",
  "expected_graph_revision": 0,
  "cell": "review",
  "expected_operation_id": "value from inspect_cell node.operation_id"
}
```

Cancellation includes descendants. Verified results stay accepted. Running work
receives cancellation intent; uncertain outcomes remain uncertain. No retry is
prepared and no saved PID is killed.

`stop_run` takes the same request and run identity fields, without `cell` or
`expected_operation_id`. It records `down` control to stop admission and request
workers to stop cooperatively. A completed run stays complete. A successful
response always reports `cleanup_confirmed: false`: inspect workers and process
evidence before assuming provider cleanup is complete.

Choose a durable `request_id` for each operation. Retry the same ID with identical
arguments after a lost response. Its original receipt is returned without another
checkpoint publication or duplicate cancellation/stop events. Reusing an ID with
different arguments is rejected. A new cell operation or graph revision requires
fresh inspection and a new request ID. Control receipts are retained for the run's
lifetime, with a maximum of 65,536 IDs; IDs are never silently recycled.

## Assembling accepted outputs

`assemble_delivery` requires a complete graph whose cells are all verified. It
does not initialize a run or resume graph work when that precondition fails.
Pass the run identity fields above, a new `request_id`, an explicit `directory`,
the selected `nodes` array and the result `field` containing a JSON file map.

The destination must be outside the durable run, and its parent must already
exist. Relative paths resolve under the MCP server's `--workspace`. File maps
are validated for path traversal, conflicting contents and file/directory
collisions. An existing destination must carry the matching manifest and exact
source contents. Assembly preserves existing checker failure or interruption
evidence rather than making a new acceptance claim.

The returned `candidate_sha256` binds the run, admitted plan, graph revision,
selected cells and source file digests. `delivery_summary` reports the same
candidate identity. Assembly returns `check_started: false` and `dispatched:
false`; it does not invoke a model or checker.

## Explicit local acceptance checks

Call `check_delivery` with the run identity fields, a new `request_id`, the exact
`expected_candidate_sha256`, an explicit `check` argv array and positive
`timeout_seconds` up to 86,400. The command runs in the candidate directory.
There is no implicit shell: use a shell explicitly if the intended command needs
one. The process receives `SWARM_DELIVERY_DIR` and `SWARM_RUN_DIR`.

Delivery and controller locks exclude competing delivery work and resumed graph
execution while candidate identity and contents are checked. Checker intent is
durably recorded before execution. Each request has separate process/output
artifacts. A successful check must also leave the accepted source contents
unchanged. The durable outcome is `accepted` or `check_failed`.

Retrying identical arguments and request ID reads the receipt without running
the command again. If only interrupted checker intent survived, the retry
returns `unknown`. `check_started: true` means execution was admitted and may
have started; it does not prove external effects or successful cleanup.
`dispatched: false` means no graph workers or model providers were launched,
not that the explicit checker had no effects.

A further check after acceptance, failure or interruption requires both a new
request ID and `recheck: true`. An active, malformed or unreadable prior process
marker blocks recheck until cleanup is reconciled. No command is silently
repeated. The delivery journal retains at most 1,024 request IDs and remains
subject to its byte limit. These operations never submit or publish a delivery.

## Saved-run compatibility

The new fenced features first validate their preconditions, then enable the
journal checkpoint format before committing new receipt state. Migration uses
the normal authority and state locks, preserves the original JSON checkpoint
backup, and publishes a schema-3 marker. Older runtimes refuse that marker rather
than silently discarding request receipts. Duplicate requests do not rewrite it.
Legacy MCP calls do not implicitly select the new lifecycle contract.
