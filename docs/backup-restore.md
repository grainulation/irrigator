# Offline backup and quarantined restore

`irrigator backup create --run /absolute/run --out /absolute/new-bundle.json`
creates a private local evidence bundle. `backup verify --in BUNDLE` checks it;
`backup restore --in BUNDLE --out NEW_DIRECTORY` restores evidence offline.
Use canonical paths without symlink ancestors (on macOS, `/private/tmp` rather
than `/tmp`). Secure backup publication and restore require a runtime built with
Go 1.26 or newer; older builds fail closed.

Creation excludes controllers, delivery, state writers and every existing task
lease with nonblocking locks. Busy sources fail; stop the original controller and
workers first. A double inventory detects changes during copying. It preserves
all regular files below the run, including original checkpoint versions, plans,
results, attempts, receipts, unknown costs and uncertain effects. It does not
migrate schemas. The bound campaign ledger is included as evidence as described below. Other
external provider/service ledgers and external artifact targets are not copied:
their original authority and reconciliation remain necessary.
This is an evidence snapshot, not a transferable execution or spending grant.

The manifest records checkpoint version, run identity, immutable plan hash,
compatibility statement and each file's length and SHA-256 digest. Payloads are
embedded, and verification rejects unsupported schemas, duplicate keys, duplicate
or unsafe paths, corruption and truncation. Digests detect accidental corruption;
they are not signatures, encryption or proof against someone rewriting the entire
bundle. Keep bundles private: run files may contain prompts, results, private
context and configured credentials. No external authentication store is read and
no secrets are redacted from run evidence.

Limits are 4,096 files, 16 MiB per file, 32 MiB total payload and 64 MiB encoded
bundle. Symlinks, hardlinks, devices, sockets and other special files are rejected.
Publication uses a synchronized temporary file and atomic no-overwrite link.
The destination parent must already exist; original paths are never overwritten.

Restore reserves a new 0700 destination and writes `RESTORE_QUARANTINE.json`
before any checkpoint bytes. It stages private evidence and publishes the
`evidence/` subtree, then writes `RESTORE_COMPLETE.json`. An interrupted restore
may leave a quarantined partial directory; it is retained, never deleted or reused.
Choose a fresh destination to retry. Files are 0600. This deliberately does not
make the whole destination appear atomically: the permanent quarantine marker
and completion receipt distinguish partial from complete restores.

Read-only commands may inspect `NEW_DIRECTORY/evidence`; checkpoint mutations,
worker admission and execution reject quarantine markers in ancestors, including
resolved symlink ancestors. Do not remove the marker or copy checkpoints out to
resume: restore never reconciles external operations or budgets. A separately
authorized new run may use accepted exported results with a new identity and
explicit provenance. The restored original identity must not be replayed.

Guarantees cover cooperating local processes using the runtime's flock discipline
and supported local filesystem sync semantics. They do not promise distributed
locking, network filesystem safety, a hostile same-user process boundary, or
protection against an operator deliberately removing quarantine evidence. No
retention or garbage collection occurs, and unresolved evidence is never removed.

Durable backup uses the `irrigator.backup/v2` envelope. Mailbox inbox,
processed cursor, receipts and outbox acknowledgements remain exact checkpoint
bytes. Campaign-bound snapshots also hold the external campaign lock after the
run locks and embed a validated ledger snapshot, bound to the original campaign
and grant IDs, run directory, immutable plan hash and run allowance. Missing,
corrupt or mismatched campaign evidence refuses backup. All grants and retained
unknown exposure remain in the captured evidence; no grant is released.

Restore writes this snapshot only to `CAMPAIGN_EVIDENCE.json` inside quarantine.
It never writes a `campaign.json` authority store or accesses the original campaign
directory. Verification and restore therefore work when the original authority is
offline. This evidence does not permit campaign admission, grant reopening or
mailbox replay. Backup itself refuses quarantined source/destination paths because
its lock acquisition would otherwise mutate preserved evidence.
