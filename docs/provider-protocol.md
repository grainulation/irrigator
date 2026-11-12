# External worker protocol

A custom provider executable is a protocol adapter, not an arbitrary shell
command. Irrigator sends one JSON `swarm.worker/v1` request on stdin and expects
one JSON response on stdout. Send diagnostic logs to stderr; extra stdout text
makes the response invalid.

The request includes `request_id`, `operation_id`, task inputs, attempt, timeout,
and the per-call reservation. Tracking does not authorize bypassing a provider
service's own enforced budget. The response must echo the protocol and the exact
request ID. For a successful task, return string-valued output fields:

```json
{
  "protocol": "swarm.worker/v1",
  "request_id": "COPY_FROM_REQUEST",
  "outcome": "settled",
  "output": {"answer": "The accepted result"},
  "cost_usd": 0
}
```

This is a wire example, not a response to replay unchanged. Preserve the actual
request identity and report any available USD estimate as a finite, nonnegative
`cost_usd`, including `0` only for explicitly reported free work. In best-effort
tracking, missing monetary cost is recorded as unavailable and does not reject
an otherwise complete result. It is never booked as a confirmed free call.
Malformed or ambiguous execution responses still require safe recovery. A report above `max_cost_usd` fails the task while
retaining the reported charge. The adapter must enforce that cap before making
provider calls: checking a receipt cannot undo spending. Validators use the
`passed` boolean instead of task output. See the authoritative
[request and response types](../internal/model/extensions.go).

A response can explicitly say `not_started`, `settled`, or `uncertain`. Use
`uncertain` if dispatch or external effects might have occurred but cannot be
confirmed. Never label a timeout `not_started` merely to enable retries. The
adapter must also enforce its advertised idempotency behavior; an operation ID
alone does not make an external action safe to repeat.

For remote executors, the same contract must preserve cancellation uncertainty,
identity, billing and artifact provenance. See [roadmap boundaries](roadmap.md).
