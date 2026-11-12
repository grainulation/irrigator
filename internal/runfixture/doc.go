// Package runfixture holds the test helpers shared by several packages.
//
// # Responsibility
//
// Run directories (New, Task, Read), execution catalogs and managed, turn, formation,
// campaign and mailbox fixtures, a deterministic FakeClock, fake tools on PATH (tmux,
// hc, git repositories, provider scripts) and golden/replay helpers. Every helper is
// built from exported contracts of lower packages only (no private state).
//
// # Allowed dependencies
//
// Unnumbered test-only package. Imports artifactstore, checkpoint, clock, cost, launch
// and model, and nothing above checkpoint, so any package above checkpoint can import it
// from its tests. Packages inside its own import closure (clock, checkpoint, launch,
// artifactstore, ...) keep private copies of the helpers they need, to avoid an import
// cycle. Third-party: none.
//
// Test-only package: production code must not import it.
//
// # Invariants
//
//   - Import it only from _test.go files.
//   - Test helpers that need private package state stay in the package under test; only helpers
//     expressible through exported contracts belong here.
package runfixture
