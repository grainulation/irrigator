# Exclusive recovery and checkpoint migration

`irrigator recover --run /source/run --in /backup.json --out /new/run`
transfers execution authority from a reachable, quiescent source to one new
private directory. It does not grant a second run budget. Ordinary `backup
restore` remains quarantined evidence and cannot authorize execution.

The source must match the backup's exact checkpoint and complete file inventory.
The command takes delivery, controller, state, every existing cell lease, then
campaign locks. A busy lease fails without retiring the source. The campaign
ledger must match its captured evidence before the transfer starts.

The durable source receipt permanently retires the source, binds one destination
path and filesystem identity, and precedes copying any executable checkpoint.
A completion receipt appears only after the files and directories are synced and
the existing campaign grant is transferred. Mutation and dispatch gates check
these receipts, path, filesystem identity, preserved checkpoint digest and source
retirement. A copied destination cannot spend the grant or execute at a new path.
Keep the source retirement directory available for these checks.

Repeat the identical command after an interruption. It accepts previously copied
files only when their bytes match and finishes the same transfer. It cannot
redirect a retired source to another target. A completed transfer is idempotent,
even after the destination advances. A crash in the initial directory-allocation
window, before source retirement is durably journaled, can leave an empty unused
directory: the source remains authoritative; choose a different new destination.
No recovery path removes or overwrites an existing user directory.

Verified outputs, attempts, histories, immutable plan/hash, run identity,
reservations and reported costs are retained. Running tasks become uncertain,
even if their legacy replay policy would otherwise permit retry. Unknown costs
and lost remote effects require explicit reconciliation; recovery never proves
a remote operation stopped and never automatically replays it. A source that is
unreachable cannot be recovered for execution; inspect a quarantined restore
instead. Recovery chains from a previously recovered target are currently
unsupported. These are local filesystem transfers, not distributed failover.

Add `--migrate-to 5` for the explicitly supported checkpoint v4 → v5 transition.
This changes checkpoint storage version only: it does not reinterpret the plan,
legacy ownership, acceptance contracts or API version. Other version jumps fail.
The exact original checkpoint is archived as `RECOVERY_ORIGINAL_STATE.json`, and
its SHA-256 is recorded alongside the backup SHA-256 in both receipts. Normal
recovery without this flag retains the original checkpoint version.


## Triage stuck cells

`irrigator triage RUN` explains every uncertain or failed cell and changes
nothing. For each cell it shows:

- whether the provider's exit was confirmed (the attempt's process record), and
  whether a worker or the controller is still running;
- reported cost, or that the cost is unknown (never shown as $0);
- the last progress lines from the attempt's log;
- for [`irrigator code`](coding-cells.md) cells: the branch, its commits beyond
  the base and their diffstat, and uncommitted changes left in the worktree;
- one recommended next step with the exact commands.

| Situation | Recommendation |
| --- | --- |
| A worker or the controller is still running | Wait, or `irrigator down RUN`; never retry while it runs |
| Code cell with uncommitted changes | Review them, commit or discard, then triage again |
| Code cell whose branch has commits | Review them; accept the branch as the result (`resolve`) or retry to continue it |
| Code cell with no work | Retry (it continues the branch), or cancel |
| Other uncertain cells | Check the provider and external system; `resolve` with the result, or retry |
| Failed cell that is safe to replay | Retry and resume |

`--confirm-stopped` appears in the commands only when the exit was not confirmed.
`--json` prints the same evidence.

`irrigator triage RUN --fix` walks through the cells at a terminal. For each it
offers only the steps that fit: commit leftover worktree changes, accept a code
cell's branch (it writes `cells/<id>/triage-result-<attempt>.json` with `branch`,
`commit`, `diffstat` and `summary`, then runs `resolve`), record a result file you
provide, retry, cancel or skip. Any step that attests the agent has stopped,
when its exit was not confirmed, needs you to type `stopped`. It refuses to start
while a controller runs this run, and offers `irrigator resume RUN` at the end.
It runs the same `resolve`, `retry` and `cancel` commands described below, so
their checks still apply; uncertain work is never replayed without your choice.

## Explicit retry and provider cleanup

`resume --retry-failed` only resets replay-safe failures. It lists skipped paid or
manual providers with an explicit `irrigator retry RUN NODE` command; skipping
is not permission to duplicate an external action.

For uncertain work, inspect the recorded attempt and external system first.
`retry --allow-uncertain` accepts the risk of duplicate effects. When provider
cleanup is unconfirmed, including older external attempts without process
evidence, `--confirm-stopped` is also required. Give that attestation only after
verifying the old provider and validator processes have stopped; the flag does
not stop processes or resolve unknown billing. Active worker leases still block
retry even with the flag.

Call timeout and per-call budget defaults are saved with new prepared/applied
runs and reused by `resume`. Explicit resume flags replace the saved defaults;
managed profile budgets and optional profile timeouts remain authoritative.
Older checkpoints without saved defaults retain the documented CLI defaults.

## Retry feedback

`retry` reports which tasks were prepared, which accepted recursive parents are
waiting for descendants, and which verified tasks were skipped unchanged. It
does not launch work; use `resume` afterward. A turn that already committed its
continuation or helpers cannot be reset to emit a second graph. Retry the failed
descendant, or use an explicit graph revision. Historical results, operations,
and enforced budget exposure remain preserved.

For a provider executable removed by an updater, see
[reviewed provider migration](provider-migration.md) instead of editing the plan.
