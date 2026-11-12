# Live session follow

`irrigator apply|resume ... --session-registry ABS` (default `<cwd>/.irrigator/current-session.json`) publishes the started run after controller ownership is acquired. There is no mtime or directory discovery.

The registry format and the event vocabulary below are the runtime's read-only
projection contract. Use `irrigator status DIR --watch` and `irrigator graph DIR`
to observe a run.

## Registry v1
`{version:1, run_id, run_path (absolute, clean), generation (monotonic >=1), started_at}`. Writes are flock-guarded and atomic. The registry file must be a regular file, not a symlink, and at most 8 KiB. Same-run republish is a no-op. A different run increments the generation, and the latest start wins. Completion never writes the registry.

A missing or malformed registry, an unreadable run, or a run_id mismatch yields `control:"unavailable"` with no run_id. A reader never reports an older run as current.

## Snapshot additions
`mode` (following|pinned), `generation`, `events`, `events_total`.

## Event vocabulary
Events come from `State.Events`, restricted to allowlisted kind/task/time. Detail is never exposed. Each event has `id = run_id:index+1` and `seq = index+1`, and is labeled `basis:"event_log"`.

| kind | action |
|---|---|
| created | run_identity |
| resumed | run_resumed |
| recovered | recovered |
| pause_requested, down_requested | control_requested |
| complete | run_complete |
| blocked | run_blocked |
| started | node_started |
| produced | node_produced |
| verified | node_verified |
| failed | node_failed |
| expanded | node_expanded |
| graph_verified | graph_verified |
| retry_requested | retry_requested |
| cancel_requested | cancel_requested |
| cancelled | node_cancelled |

Clients dedup by id and order by seq. Filtered kinds create normal seq gaps and are not loss. A decrease in events_total, a run_id change, or a window loss (no overlap with previously seen ids) resets the reducer with no historical cues.

The node transitions above are emitted as allowlisted events. spawn and handoff have no emitter and are not shown.

## Limits
The upstream event log carries typed per-node records for the allowlisted kinds above, but no handoff record. Events are the last 200 allowlisted entries, and the log is not guaranteed to be complete history.
