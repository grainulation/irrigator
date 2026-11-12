# Local and hosted execution architecture

This plan separates execution policy from placement while keeping local execution
the default. Priorities, in order: correct logic, a fast local path, and interfaces
that maintainers can understand and test. It adds no required hosting service.

## Decisions

| Boundary | Local implementation | Hosted implementation must provide |
| --- | --- | --- |
| Run transactions | Validated checkpoint under existing filesystem lock | Atomic state, admission, reservation and event commit; conflict and ambiguous-commit handling |
| Attempt execution | Supervised local adapters | Exact admitted identity, bounded inputs/outputs, explicit certainty and cost, authenticated transport |
| Artifact storage | Private staged files with verified content identity | Scope-bound immutable objects, checked length/digest, authorized reads and writes |
| Ownership | Existing controller/cell leases and inherited process descriptor | Epoch fencing checked in the state transaction, renewal and supervised loss-of-ownership handling |
| Authority | Existing local campaign and billing checks | Authenticated tenant scope and transactional cross-run budget authority |

The first three are implementation boundaries. The last two describe requirements
for a hosted coordinator; local locks and scope names do not implement them.
These are internal Go contracts, not a public remote API or selectable cloud
backend. Artifact storage is currently wired to mapped export files; aggregate
manifests and existing inline worker results preserve their current formats.
Large remote artifact references in admitted plans require a separate compatible
protocol/storage evolution before they can replace those inline results.

## Logic: preserve one authority for each decision

Admission, attempt identity and budget reservation remain a single transaction.
Outcome acceptance checks the admitted run, task, operation, attempt and binding.
Provider execution happens outside the state transaction. A timeout or lost
response remains uncertain; neither a retryable database error nor a new worker
creates permission to repeat an external action.

Transaction callbacks execute at most once per call. Existing callbacks rely on
local lease and preparation state; a backend must not silently replay them after
a conflict. A future database backend may retry a separately prepared pure
mutation, with an operation identity and expected revision, after all relevant
preconditions are revalidated. An error after commit can mean the result was
published but its acknowledgement was lost. Reconcile by identity before retry.

For hosted dispatch, admission and a dispatch intent must commit together. A
relay may deliver that intent more than once, but the worker must deduplicate the
operation and preserve uncertain effects. The current mailbox acknowledgement
outbox is not this provider-dispatch outbox. Do not treat it as one.

The executor receives work that was already admitted. Its receipt cannot choose
a different identity or increase its authority. Transport must preserve explicit
unknown cost and usage: internal-only worker response fields are
not a portable receipt format. Keep local paths, executable configuration and
credentials in the local adapter rather than in the portable work envelope.

Artifact identity includes run scope, a canonical key, byte length and SHA-256.
Storage verifies the content rather than trusting a claimed hash. Published
content is immutable. A scope is an isolation key, not authentication: a hosted
service must derive and authorize it from the caller's authenticated identity.

## Speed: keep overhead proportional to work

Local execution calls local implementations directly; no HTTP hop, daemon or
additional artifact cache is introduced for abstraction alone. Preserve existing
checkpoint and export formats. Avoid mutable decoded-state caches until invalidation
across processes and external authority changes is proven correct.

Measure checkpoint read/update cost separately from provider latency and artifact
hash/write cost. Benchmarks are local measurements, not hosted throughput claims.
Use bounded payloads and immutable artifact references for large future remote
inputs rather than copying whole run histories. A hosted scheduler should use
change notification or durable queues instead of replicating the local polling
loop across network boundaries.

## Maintenance: small interfaces, one semantics suite

Keep adapters explicit and injected; avoid mutable global backend registries or
an interface for every helper. Preserve plan hashes, checkpoint versions, existing
worker protocol and artifact filenames. Conformance checks cover rollback,
concurrent updates, stale identity, uncertainty, content mismatch and scope
isolation. Alternative test adapters must exercise real orchestration seams, not
only compile against an interface.

Local lifetime process leases remain local capabilities. A TTL lease is not a
replacement unless every state-changing commit checks its fencing epoch and
worker cleanup handles lost ownership. Preserve the existing supervisor behavior
throughout extraction.

## Delivery order and acceptance

1. Extract and wire checkpoint, executor and artifact boundaries into the existing
   runtime. Test local behavior and alternate adapters; preserve compatibility.
2. Before selecting a hosted coordinator, prepare pure mutations and a durable
   provider-dispatch outbox; bind ownership epochs to transactional acceptance.
   Prove concurrent admission cannot spend one shared allowance twice.
3. Implement one hosted backend behind those contracts. A transactional database
   and immutable object storage are a straightforward baseline, not dependencies
   selected by this plan. Keep worker placement independently configurable.
4. Add authenticated tenant/run/worker authority, credential delivery, revocation,
   quotas and audit records. Test cross-tenant denial through the real API.
5. Exercise partitions, duplicate delivery, lost acknowledgements, host death,
   cancellation races, artifact corruption and schema upgrades in the deployed
   topology before calling the hosted coordinator supported.

## Reusable core

The current domain types still live in the CLI package. The next extraction is a
small importable core containing domain types and pure transition/acceptance
logic; the CLI and a future service should call that same core. Keep process,
storage, credential and terminal concerns in adapters. Go interfaces alone do
not establish portability across programming languages.

A public library API should follow a second real backend and the shared
conformance suite, so supported APIs reflect tested needs. Other languages use a
versioned worker/service protocol rather than reimplementing the state machine.
Release compatibility, ownership and authorization guarantees must be documented
before that API is described as usable across hosting environments.

This change prepares boundaries; it does not make the whole coordinator remotely
selectable. Recovery, campaign authority, billing allocation, process supervision
and revision preparation still contain local implementation requirements. Their
migration must follow the same contracts rather than bypass them.

## Current technical basis

Checked against primary documentation in October 2026:

- [Go transaction guidance](https://go.dev/doc/database/execute-transactions):
  keep related reads and writes in the same transaction and handle commit errors.
- [PostgreSQL isolation](https://www.postgresql.org/docs/current/transaction-iso.html):
  serializable transactions can fail and require application retry handling.
  Such retries do not authorize replay of provider side effects.
- [S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
  and [integrity checks](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html):
  object creation/preconditions and content verification are distinct requirements;
  an object ETag alone is not a cross-resource transaction.
- [Temporal activity guidance](https://github.com/temporalio/documentation/blob/main/docs/encyclopedia/activities/activity-definition.mdx):
  retryable external work needs idempotent application behavior. Durable workflow
  state does not make an arbitrary model call or external write exactly once.

These are design inputs, not endorsements of a required vendor or claims that an
Irrigator backend for those systems has been implemented.
