// Package branchreview is the command-side branch and change review workflow.
//
// # Responsibility
//
// The review CLI, change review (queue, diff, verdicts, comments, feedback) and the terminal-UI
// backend that feeds internal/reviewtui. It imports reviewtui only through its Backend
// interface.
//
// # Allowed dependencies
//
// Layer 25. May import only lower layers; today it imports checkpoint, commandflags, jsonio,
// launch, localfs, messaging, review, reviewtui, session, terminalview, textsafe. Third-party:
// none. The compiler rejects import cycles; the layer order is recorded in
// docs/architecture.md.
//
// # Invariants
//
//   - A verdict is bound to the exact head commit it was made on.
//   - Review state is updated under an exclusive lock and replaced atomically.
//   - Diff text is sanitized for terminals; the unsanitized bytes are available separately
//     (rawChangeDiff) for storage and hashing.
package branchreview
