# Session registry v2 and multi-session projection

One shared absolute path, e.g. `/ABSOLUTE/PATH/TO/.irrigator/current-session.json` (a placeholder; choose your own location), is used by
`irrigator apply|resume --session-registry ABS`.

Observe runs in the terminal with `irrigator status DIR --watch` and `irrigator graph DIR`.

## File format (version 2, max 64 KiB, regular non-symlink file)

```json
{"version":2,"generation":7,"current_run_id":"...","updated_at":"...",
 "entries":[{"run_id":"...","run_path":"/abs/run","started_at":"...","last_started_at":"...","generation":7}],
 "overflow":{"recent_evicted":0,"active_rejected":0}}
```

- Every actual start upserts under `<registry>.lock` and is written atomically. No later start overwrites another run; `current_run_id` is only the latest start.
- v1 files remain readable (one entry). The next actual start of any run rewrites v2 and preserves the v1 run, its `started_at` and its path. Even a same-run start migrates.
- Bounds: at most 16 recent entries, 32 active entries, 64 entries per file. Recent eviction is counted in `overflow.recent_evicted` and `last_evicted_run_id`. Active runs are never silently evicted. Runs with unreadable state are treated as active; runs with missing state are evictable.
- Capacity admission: every start (new run, resume of a recent run, or an idempotent same-run restart) counts the OTHER active runs, excluding its own entry, before any early return. If those others already number 32, the start is rejected, `apply` warns, and `overflow.active_rejected` / `last_rejected_run_id` record it. An active run at cap may restart while the other active runs number 31 or fewer.
- A corrupt registry fails closed with an error and leaves the file untouched. A run id registered under a different path is refused.

## Derived status (never from chat)

`complete` needs `control=complete` plus every node verified, with no live lease. Otherwise `inconsistent`. Other statuses: `running` (live lease, or `control=run` with a held `controller.lock`), `stalled` (`run` with no controller), `paused`, `blocked`, `ready`, `indeterminate` (lock probe error), `missing` and `unavailable`. Probes are read-only and non-blocking.

## Multi-session summary projection

Session summaries expose: `sessions[]` (current first, then active, then others; counts, status, workers, up to 32 agents of id/parent/status/attempt, `unavailable_reason`), `sessions_total/limit/truncated`, `registry_version`, `registry_overflow`, `current_run_id`, `selected_run_id`.
If the selected run is unreadable, status is `unavailable`. There is no stale-current fallback.

## Per-session event tail

Each `sessions[]` summary carries:

- `events`: at most 64 safe events, the most recent of the run's event log.
- `events_floor`: the `seq` of the first returned event, or `0` when none are returned.
- `events_watermark`: `len(State.Events)` at read time.
- `events_total`: unchanged, `len(State.Events)` at read time.

Each event has `id` = `run_id:seq`, `seq` = 1-based index into the append-only `State.Events`, `kind`, `task`, `time`, `action`, `basis`. Only allowlisted event kinds are exposed; `Detail` is never exposed. Unavailable or unreadable sessions have `events: []`, `events_floor: 0`, `events_watermark: 0`.

Semantics:

- `seq` gaps are normal: unmapped event kinds are filtered out and are not losses.
- The log is not complete durable history. It is a bounded window of the newest 64 safe events per read.
- Clients take the first snapshot as a baseline and dedupe by `run_id:seq`.
- Scoped window loss: if events were appended between reads and fell outside the 64-event tail, they are not delivered. A client detects this when its last seen `seq` is below `events_floor` and it has not seen the intervening events. The watermark shows how far the log had advanced at read time.

## Cost and bounds

These are two separate properties:

- Cost: each snapshot scans every registered run's state and all of its tasks, and performs lock probes. Cost therefore scales with registered state and task count, not with the output size.
- Bounded output: the serialized `sessions[]` is limited to 24 sessions, with up to 32 agents and 64 events per session. Only the serialized summary tail and agents are bounded; the scan is not.

No performance or scale claim is made for large registries or task counts.
