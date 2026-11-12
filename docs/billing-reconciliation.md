# External billing reconciliation

Checkpoint5 runs support an explicit operator attestation for USD that a provider did not report. This is not automatic invoice verification, a token-price estimate, or a way to resolve uncertain execution. Native swarm/v6 runs and explicitly migrated checkpoints can use it.

`irrigator billing reconcile RUN RECEIPT.json --attest-external-billing`

Every field below is required; null, aliases, duplicates and unknown fields are rejected. Use the exact run, operation, attempt and binding from the checkpoint. Amounts are integer microdollars (1 USD = 1000000); zero must be explicit and supported by your evidence.

```json
{
  "schema": "irrigator.billing/v1",
  "run_id": "EXACT_RUN_ID",
  "task_id": "EXACT_TASK_ID",
  "operation_id": "EXACT_OPERATION_ID",
  "attempt": 1,
  "binding_digest": "EXACT_BINDING_DIGEST",
  "receipt_id": "provider/account/invoice/unique-charge-line",
  "currency": "USD",
  "amount_micros": 12000,
  "evidence_sha256": "SHA256_OF_EXTERNAL_BILLING_EVIDENCE",
  "evidence_reference": "Private invoice reference; do not include credentials",
  "attestation": "operator_verified_external_billing"
}
```

The operator verifies the external evidence and its digest. Irrigator retains the attestation and digest, not a copy of a potentially sensitive invoice. The original provider report, unknown-cost flag, operation history and immutable plan stay unchanged. A separate external-billing total distinguishes this evidence from provider-reported charges.

An unknown-cost reservation uses the reconciled amount for subsequent admission. An uncertain execution continues to hold at least its full reservation; billing cannot establish that a request did not execute. Overruns freeze further admission. Repeated identical receipts are idempotent; corrections require a future audited adjustment mechanism rather than editing an existing receipt.

A private installation-local allocation registry reserves each globally unique charge-line ID before the checkpoint transaction commits. A crash can strand an allocation, but cannot safely allocate it to another operation; retry the identical receipt. The default registry is `Irrigator/billing` under the OS user configuration directory. `IRRIGATOR_BILLING_REGISTRY` selects an explicit isolated namespace for testing or a managed installation. Deduplication applies within that registry, not across separately configured installations. Use one durable registry for runs sharing a billing account. An invoice can cover multiple charge lines, each with its own receipt ID.

The checkpoint records its registry path. Missing or mismatched allocations block mutations, dispatch and campaign closure; read-only evidence inspection remains possible. Backup/restore does not mint replacement billing authority. Preserve the registry along with the source during recovery. This is a cooperative local-user security boundary, not protection against an administrator rewriting files.
