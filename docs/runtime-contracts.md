# Runtime contracts

Runtime contracts are opt-in admission requirements and evidence export layered
on the checkpoint engine. They do not replace checkpoints with SQLite, grant
tool permissions, or claim an all-purpose safety envelope.

## Admission

A `swarm/v6` plan can include:

```json
"guardrails": {
  "schema": "irrigator.guardrails/v1",
  "require": ["managed_binding_validation", "attempt_fencing", "unknown_cost_holds", "bounded_artifact_validation"],
  "max_inflight": 3
}
```

Omitting the field leaves plan encoding and behavior unchanged. The guardrails become
part of the immutable admitted plan and its hash. Binaries that predate the field do not support
plans containing it. Requirements are checked during plan validation and
again inside the checkpoint transaction immediately before attempt admission.
Unknown, duplicated and unsupported requirements fail closed. Requirements name
narrow implementation contracts; read each reported limitation before relying on
one. A capability declaration is not a live security attestation.

`max_inflight` is a hard local admission bound on unresolved operation/attempt
identities. Running, produced and uncertain attempts, unknown task costs and
unsettled managed reservations retain slots. New generations do not reuse an old
operation's slot. It is independent of controller worker count. It bounds neither
CPU/memory nor provider billing, and can intentionally block work until explicit
reconciliation. It does not reserve separate verification capacity. The existing
supervisor/cell lease is still responsible for execution ownership.

## Inspect and export

```sh
irrigator contracts plan PLAN.json
irrigator contracts receipt RUN_DIRECTORY
```

Both commands are read-only; neither calls a provider, starts workers, probes saved
PIDs, approves an action or performs cleanup. Plan inspection reports supported
and unsupported implementation contracts. Receipt export reports the checkpoint
snapshot's run/plan identity, digest, sorted attempt and reservation identities,
reported costs, uncertain statuses and accounting freeze state. It excludes
prompts, outputs, raw diagnostics and credentials. Capture time is intentionally
omitted so identical snapshots produce identical digests.

The `sha256` covers the compact JSON encoding with `sha256` set to the empty
string. `state_sha256` identifies the source snapshot, not a live process. These
are integrity digests, not signatures or independent attestations. Archive the
receipt with its run evidence if provenance is needed. Export does not consume or
release any reservation and does not turn verification into acceptance.

## Nine contract areas

| Area | Implemented baseline | Remaining boundary |
| --- | --- | --- |
| Adapter capability checks | Pinned managed bindings plus explicit admission requirements | No complete mediation or generic isolation attestation |
| Budget reconciliation | Exact monetary holds, unknown costs, overrun freeze; `hard_token_limit`: explicit output-generation ceilings (`max_output_tokens`) on trusted local Ollama profiles | Not a prompt-token, whole-run, time or monetary ceiling; relies on the server honoring `num_predict`; other providers still reject the requirement |
| Cancellation | Supervisor, group TERM/KILL and unresolved cleanup evidence | Detached processes and remote generation are outside the group |
| Recovery snapshots | Authority recovery and retained uncertain evidence; `dirty_worktree_quarantine`: an explicit Git quarantine command captures recovery objects and preserves the worktree on disk | The operator must confirm writers stopped; agent worktrees are not captured automatically |
| Approvals | Signed single-use POST links; lock-time expiry check and mint-time task attempt binding | Older links remain readable; task identity is not an exact tool-payload grant |
| Settlement | Fenced checkpoint results; `recoverable_git_settlement`: explicit prepare, verify, accept and settle of a candidate with a durable intent journal and conditional ref advancement | Only `refs/irrigator/integration/*` targets; no automatic checkout sync or rollback of external effects |
| Immutable verification | Bounded artifacts, fenced validation and Review snapshots | No generic integration-tree verification implied by an OID string |
| Resource admission | Transactional `max_inflight` bound retains unknown-operation slots; `host_resource_limits`: per-provider container CPU/memory settings, checked against the runtime before launch | Runtime-specific and needs a sandboxed provider; no uncontained fallback; no dedicated verification reserve |
| Evidence receipts | Deterministic redacted snapshot projection | Not proof of external completion or invoice settlement |

`dirty_worktree_quarantine`, `recoverable_git_settlement`, `host_resource_limits`
and `hard_token_limit` are accepted only within the narrow scopes above; a plan
that requests one its configuration cannot honor is rejected before the run is
created. `automatic_worktree_quarantine`, `automatic_git_settlement` and
`complete_tool_mediation` remain unsupported and are rejected; none can be
enabled by asserting an adapter capability string.

The Git contracts are driven explicitly with
`irrigator contracts git quarantine|prepare|accept|settle|reconcile`; nothing quarantines
or settles a worktree automatically.

## Approval changes

Task links minted by the current binary include operation, generation and attempt identity.
Decisions recheck expiry after acquiring the approval lock. Bound ledger
links also hold the checkpoint lock through identity validation and append;
mailbox links enqueue the mint-time identity so concurrent retries are handled by
the stale-message fence. Older signed links keep their original scope.
No raw token is included in contract receipts. Late outcome evidence and provider
replay rules are unchanged. This does not bind individual ACP payloads or force a
cooperative agent to request approval.

## Validation

Tests cover unavailable requirement rejection before run creation, byte-for-byte plan
compatibility, retained unknown slots, new-generation capacity fencing, immutable
receipt identity/redaction, decision-lock expiry and stale approval attempts.
