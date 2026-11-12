// Package managed implements the managed provider adapters and managed-execution budgeting.
//
// # Responsibility
//
// Managed codex and ollama adapters, managed history derived from the admitted checkpoint,
// managed initialization, and the budget reservation and settlement of managed execution.
// Managed work turns a model.WorkRequest into a model.WorkResponse through an adapter.
//
// # Allowed dependencies
//
// Layer 11. May import only lower layers; today it imports artifactstore, checkpoint, clock,
// cost, jsonio, launch, localfs, model, providerenv. Third-party: none. The compiler rejects
// import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Managed receipts attest adapter delivery, never model comprehension or vendor identity.
//   - Provider-reported token accounting (codexTokenUsage) is never treated as a USD estimate.
//   - Managed history is derived only from the admitted checkpoint snapshot.
//   - Managed initialization inspects a local executable but never launches it.
//   - A reported cost is read from the raw JSON number, never through float64, and converted
//     exactly to micro-dollars.
package managed
