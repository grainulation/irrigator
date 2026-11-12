# Durable local mailbox

Runs admitted with a mailbox use durable mailbox messaging. Checkpoints
and admitted plans remain immutable.

```sh
irrigator mailbox enqueue RUN message.json
irrigator mailbox process RUN 64
irrigator mailbox outbox RUN
irrigator mailbox ack RUN 1
```

The controller also drains bounded batches through its mailbox hook. Enqueue and
processing are separate transactions. Enqueue prints the stable sequence number;
processing commits the local state change, receipt, FIFO cursor, and outbound
notification in the same atomic checkpoint replacement under the run state lock.
If a caller loses the response after a commit, it resubmits the same message key
and payload and receives the existing sequence. A duplicate key with any changed
field is rejected. Processing a committed message again does nothing.

A message file contains these fields, copied from the exact current run/task
identity in `state.json`:

```json
{
  "schema": "irrigator.message/v1",
  "key": "cancelFirstAttempt",
  "run_id": "EXACT_RUN_ID",
  "task_id": "a",
  "operation_id": "EXACT_64_HEX_OPERATION_ID",
  "generation": 1,
  "attempt": 1,
  "kind": "cancel",
  "payload": "User requested cancellation."
}
```

The example identity placeholders must be replaced. `key` is a simple identifier
beginning with a letter, at most 64 characters. Payloads are UTF-8 strings of at
most 4096 bytes; message JSON is bounded to 16 KiB and rejects duplicate keys,
unknown fields, and trailing data. Only regular files are read.

There are two local command kinds:

- `signal` records a receipt and retains the payload in the inbox. It does not
  inject provider context, resume a cell, admit new work, or dispatch a model.
- `cancel` requests cancellation of the single addressed task. Pending, waiting,
  and failed tasks become cancelled. Running, produced, and uncertain tasks keep
  their current status, results, costs, and uncertainty while recording cancellation
  intent. Verified and already cancelled tasks remain unchanged. Descendants are
  not implicitly targeted.

Every command checks the run ID, task ID, operation ID, generation, and attempt
at processing time. A mismatch produces a durable `stale` receipt without changing
the task. This handles messages that arrive after retry, revision, or completion
without granting authority over a newer attempt. A cancellation does not prove
that an external effect stopped, settle costs, clear uncertainty, or enable replay.

`outbox` lists unacknowledged notifications containing sequence, key, message
digest, and disposition. An external consumer may receive these repeatedly and
must deduplicate by run identity and sequence. `ack` durably marks one existing
notification acknowledged; repeating the acknowledgement succeeds. Notifications
and deduplication tombstones remain in the checkpoint after acknowledgement.
There is no network dispatcher or exactly-once vendor-effect promise.

Ordering is FIFO within one run mailbox; it is not a global ordering across runs,
providers, or other graph transactions. The mailbox admits at most 1024 messages
**over the run's lifetime**, including processed/acknowledged messages. At that
bound, new keys fail closed; exact duplicates still succeed. There is deliberately
no pruning or automatic capacity reset that could lose deduplication history.
Per-processing batch limits range from 1 to 1024 (CLI default 64).
