// Package backup creates and restores run backups and recovery transfers.
//
// # Responsibility
//
// Run backup creation and reading, recovery transfer, campaign backups and backup path rules.
// RecoveryTransfer takes a crash-injection hook so each stage can be tested; each named stage
// is durable before the hook runs.
//
// # Allowed dependencies
//
// Layer 18. May import only lower layers; today it imports checkpoint, commandflags, jsonio,
// launch, managed, model. Third-party: none. The compiler rejects import cycles; the layer
// order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - A backup is trusted only after validation, never because it exists (the store package
//     enforces this for checkpoint backups).
//   - Each named recovery stage is durable before its test hook runs, so a crash at any stage
//     is reproducible.
//   - A copied checkpoint has no authority at a different filesystem identity or path;
//     restoring goes through this package and the checkpoint recovery guards.
package backup
