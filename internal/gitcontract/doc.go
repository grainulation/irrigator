// Package gitcontract captures, quarantines and settles repository state for tasks.
//
// # Responsibility
//
// Explicit local Git operations used by git contracts (capture, verify, settle, quarantine).
//
// # Allowed dependencies
//
// Layer 26. May import only lower layers; today it imports checkpoint, commandflags, jsonio,
// launch, localfs, session. Third-party: none. The compiler rejects import cycles; the layer
// order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - These operations do not mutate a run checkpoint, replay providers, prune worktrees or
//     advance a developer's checked-out branch.
//   - Capture requires stopped writers and rejects submodules and symlinks.
//   - Verification is an explicit local command, not a model assertion.
package gitcontract
