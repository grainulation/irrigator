# Session demo: transitions and history

This page describes session registry transitions, retirement rules, and history tracking implemented in `session_registry_v2.go` and `internal/session/session_retirement.go`. Observe transitions with `status --watch`.

## Demo sequence (credential-free, no paid calls)

Use an absolute registry path and the mock backend. `REG` and `DIR` are placeholders you choose.

```
irrigator apply PLAN --run DIR/a --backend mock --session-registry REG
irrigator status DIR/a --watch
irrigator pause DIR/a
irrigator retire RUN_ID --reason "parked while rewriting" --session-registry REG
irrigator resume DIR/a --session-registry REG
```

- `pause` only changes `control`; the session is shown as `paused` and stays active.
- `retire` is explicit and registry-only. Run state and checkpoints are not modified (the only side effect is creating empty lock files).
- `resume` calls the same publish path as `apply`, so it reactivates a retired run and appends a `reactivated` history record. Generation counts it as a start.

## State and visibility rules

| Situation | Status shown | Counts as active | Auto-hidden or retired |
|---|---|---|---|
| `control=pause` or `down`, no live worker | `paused` | yes | never |
| State unreadable, or run_id mismatch | `unavailable` | yes | never |
| Lock probe error | `indeterminate` | yes | never |
| `ready`, `running`, `stalled` | as named | yes | never |
| `complete`, `blocked`, `inconsistent`, `missing` | as named | no (recent) | only dropped from the registry beyond 16 recent rows, with `overflow.recent_evicted` recorded |
| Explicitly retired and fully stopped | execution status unchanged | no | `visibility=retired` |
| Retired but live or unknown | `visibility=active` with note `live_or_unknown_after_retirement` | yes (protected) | retirement record kept in history, not presented as retired |

Retirement never changes the execution status. A retired paused run still reads `paused`.

## Retirement refusals (nothing is changed)

Retire is refused when the registry is absent or malformed, the run is unregistered or already retired, `--superseded-by` is not a different registered non-retired run, the reason is empty, over 160 characters or contains control characters, a controller or worker lock is held, state cannot be read or its run_id differs, or any node is `running`, `produced` or `uncertain`. Uncertainty is never retired automatically; resolve it first.

## Bounds

32 active, 16 recent, 16 retired, 6 history records per entry (older ones counted in `history_dropped`), 64 entries and 64 KiB per file. When 32 other active runs exist a new start is rejected and counted in `overflow.active_rejected`. Entries over the retired bound are evicted oldest first and counted in `overflow.retired_evicted`. The active-full overflow counter is written through the bounded writer, so it cannot push the file past 64 KiB.
