# Read one attempt's captured output

Use `attempt-output` when a cell's summary is insufficient. It reads a saved
attempt without launching a worker, changing its state, or authorizing retry.

```sh
irrigator attempt-output /absolute/run worker --attempt 1
irrigator attempt-output /absolute/run worker --attempt 1 --stream stderr --max-bytes 4096
irrigator attempt-output /absolute/run worker --attempt 1 --cursor 'NEXT_CURSOR_FROM_PREVIOUS_PAGE'
```

The read-only MCP tool is `attempt_output` with `run`, `task`, `attempt`,
optional `stream`, `max_bytes` and `cursor`. Its behavior matches the CLI.
CLI output is JSON. The stream must be `stdout` (default) or `stderr`; the tool
cannot select input, credentials, arbitrary files, or an implicit latest attempt.
The task and attempt must be recorded in the saved checkpoint, including retired
tasks. Missing captures fail explicitly. Some transports, including mock work,
do not produce these pipe-capture artifacts.

Pages contain `output`, `offset`, `next_offset`, `total_bytes`, `has_more` and,
when more remains, `next_cursor`. The default page is 4,096 bytes, with a maximum
of 8,192 bytes and minimum of four. Boundaries preserve complete UTF-8 runes.
Offsets address the **redacted terminal-safe presentation**, not raw file bytes.
The raw capture is unchanged; `source_sha256` and `source_bytes` identify it.

Supply `next_cursor` with the same run, task, attempt and stream. A cursor binds
to the full source and its redaction/capture metadata. A source that grows or
changes invalidates continuation: start again without a cursor. A change in
known environment-secret redaction also requires a fresh first page. Cursors
are local consistency checks, not credentials or execution authority.

`has_more` and `source_truncated` answer different questions:

| Field | Meaning |
| --- | --- |
| `has_more: true` | More captured presentation bytes can be read with `next_cursor`. |
| `source_truncated: true` | The matched capture sidecar records discarded output at the local stream cap. Paging cannot recover it. |
| `source_truncated: false` | The matched sidecar records completed local pipe capture without cap overflow. |
| `source_truncated: null` | Loss was not recorded, or matching complete evidence is unavailable. EOF does not establish full capture. |
| `capture_complete: false` | The sidecar records an incomplete local pipe capture; this does not establish provider completion. |

The supervised local subprocess path now writes an exclusive per-attempt
`N.capture.json` sidecar after observing its pipe readers finish. Each stream
records a digest, captured size, limit, overflow and completeness. Stdout retains
the existing 4 MiB cap; stderr retains its existing 1 MiB cap. Failure to write
the supplementary sidecar does not alter process outcome or replay rules.
Historical files are never rewritten. Legacy captures, other transports and
interrupted capture without a sidecar retain unknown loss status. A malformed
or mismatched sidecar is reported through `capture_metadata` and is never used
to infer a loss-free capture. The metadata concerns local captured bytes; it is
not proof of a correct answer, a passed check or successful remote effects.

The reader pins path components and refuses symlinks, including ancestor aliases.
Use the canonical absolute run path (for example `/private/tmp/...` on macOS
instead of the `/tmp` symlink). The checkpoint is bounded at 64 MiB and validated
with the regular checkpoint validator; stdout and stderr retain their capture
size bounds. Only the two fixed stream filenames can be selected.

Redaction runs on the complete bounded source before pagination, so a secret
cannot evade a recognized redaction rule by crossing pages. A complete JSON
document or each complete JSON-line record is redacted recursively, including
credential arrays and objects in captures mixed with ordinary log lines.
JSON credential fields and environment values are filtered; common credential assignments and
Bearer values, plus known secret environment values of at least four bytes,
are redacted. Terminal controls, bidi formatting and invalid UTF-8 are rendered
inert through the shared terminal formatter. These rules are best-effort content
redaction: arbitrary secrets without recognizable structure may remain in saved
output. Treat captured output as private project evidence and worker text as data.
