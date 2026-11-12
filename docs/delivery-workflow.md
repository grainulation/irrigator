# Delivery workflow

One journal, `RUN/delivery.json`, extends the existing controller; there is no second supervisor. `deliver`
resumes the graph, assembles a delivery directory, optionally runs an acceptance command, and journals each step.

## Distinct facts

| Journal status | Meaning | Not implied |
|---|---|---|
| (graph) `verified` | Model output passed its own validators | delivered or accepted |
| `exported` | Files were extracted into the delivery directory, manifest hashes match | acceptance ran or passed |
| `checking` | Acceptance command started; outcome unknown if no live deliver holds `delivery.lock` | pass or fail |
| `check_failed` | Acceptance failed or cleanup is unconfirmed | safe to rerun automatically |
| `accepted` | Check passed (or none was configured: `delivery-status` reports `not_configured`) and sources were unchanged | product approval |

## Rules

- A failed or interrupted check is rerun only by explicit `--recheck`. A failed pre-check step during recheck keeps the gate.
- A journaled `accepted` record is not downgraded by transient preconditions (controller lock busy, graph not complete,
  node no longer verified). `deliver` fails, the journal is untouched, and `delivery-status` reads `stale` or `blocked`.
  A later plain `deliver` returns the existing acceptance without rerunning the check.
- Uncertain nodes are never retried by `deliver`; reconcile with `resolve`. Delivery reads `blocked / reconcile_external`.
- Revision-bound statuses (`accepted`, `exported`, `checking`, `check_failed`) cannot be reused across a graph revision.
- Tampered delivery files fail assembly; they are never reported as accepted.
- `delivery-status RUN_DIR` is read-only, makes no model call, and prints only `stage`, `acceptance`, `recovery`.

## Known limitation

If delivered files are modified after acceptance, the journal moves to `export_failed`. Restoring the files byte-for-byte allows a plain `deliver` to rerun acceptance on the new export.

## Tests

Run the delivery tests:

```sh
go test -tags integration -run 'ParityDelivery|Delivery' ./...
```
