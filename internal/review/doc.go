// Package review holds the review snapshot primitives.
//
// # Responsibility
//
// Snapshot types (File, Blob, Comment, Snapshot), snapshot identity, bounded snapshot read and
// write, and digest and Git-blob checks shared by the review commands.
//
// # Allowed dependencies
//
// Unnumbered: it sits outside the numbered layer table and was not changed by the package
// split. May import only lower layers; today it imports localfs, textsafe. Third-party: none.
//
// # Invariants
//
//   - Snapshot reads are bounded by snapshotLimit.
//   - Terminal text and paths are validated through textsafe and localfs rather than ad hoc.
package review
