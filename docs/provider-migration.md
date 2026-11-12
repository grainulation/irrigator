# Explicit provider executable migration

A managed run pins its provider executable path and SHA-256. If an updater removes
that executable, Irrigator refuses to substitute another one automatically.
Executable migration lets an operator approve a replacement for pending and
future work without editing the admitted plan or historical receipts.

Pause the run, wait for its controller and workers to stop, and reconcile any
running, produced, or uncertain tasks first. For a settled failed task, explicitly
retry it to pending before preparing the migration. Preparation never starts the
replacement executable, checks authentication, or makes a provider call.

```sh
irrigator pause ./runs/review
irrigator profiles migrate-prepare \
  --run ./runs/review --profile default \
  --executable /absolute/path/to/new/claude \
  --out ./runs/provider-migration.json
```

Review the proposal's old binding, replacement executable path and SHA-256,
unchanged model/tools/credentials/limits, and pending-task list. The printed
`approval_digest` identifies the complete proposal. Then apply that exact digest:

```sh
irrigator profiles migrate-apply \
  --run ./runs/review --proposal ./runs/provider-migration.json \
  --approve COPY_THE_REVIEWED_DIGEST --confirm-stopped
irrigator resume ./runs/review
```

Apply takes the controller lock and every cell lease, checks cleanup evidence,
re-verifies the executable, and rejects proposals if the run changed after
preparation. `--confirm-stopped` is an operator attestation for older/missing
process evidence; it cannot override an active or malformed process marker.
The run remains paused after migration. It never reruns completed work.

The original plan, plan hash, original binding catalog, accepted results,
operation identities, past attempt files, costs, and held reservations remain
unchanged. An append-only migration record names the old/new binding and approval
digest. Pending work and subsequently generated tasks use the latest approved
binding; historical attempts retain their own binding identities. Explicit later
retries retain old accounting and select the current approved binding.

Current support is deliberately narrow: Claude and Codex CLI profiles only,
with at least one pending task. Campaign-bound runs and profiles with explicit
conversation history are rejected. Model, provider kind, credentials, tools,
timeouts, and budgets cannot be changed by this command. There are at most 64
recorded migrations per run. Backups retain migration provenance and remain
quarantined when restored.

SHA-256 verification establishes the selected executable's bytes, not vendor
compatibility or trust. No compatibility probe is run; review and test a new CLI
version separately before approving it. Older Irrigator versions reject the new
`provider_migrations` checkpoint field instead of silently ignoring the change.
Do not remove that field or edit bindings to make an old binary accept the run.
