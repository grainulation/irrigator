// Package store is checkpoint backup creation and validation.
//
// # Responsibility
//
// EnsureBackup creates an idempotent, non-overwriting backup (path.bak) of a checkpoint; a
// pre-migration alias beside it satisfies the same purpose. Existing candidates are validated
// rather than trusted.
//
// # Allowed dependencies
//
// Unnumbered: it sits outside the numbered layer table and was not changed by the package
// split. Imports no other internal package. Third-party: none.
//
// # Invariants
//
//   - A backup is trusted only after it is validated: bounded, non-empty, regular, non-symlink,
//     one complete checkpoint JSON document whose run and plan identity agree with the live
//     checkpoint where both carry them.
//   - A rejected existing backup is evidence: it is never modified, replaced or removed, and
//     every rejection wraps ErrSuspectBackup.
//   - Every checkpoint or backup read is bounded; larger files are treated as suspect and never
//     loaded.
//   - Leaf package: standard library only.
package store
