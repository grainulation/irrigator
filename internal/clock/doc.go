// Package clock supplies the time source for code whose deadlines must be testable.
//
// # Responsibility
//
// Clock bundles Now, Sleep and After so every deadline calculation (grace windows, drain
// limits, process-gone waits) reads one source: substituting a clock moves deadlines and waits
// together. The package owns the process-wide default (ActiveClock) and the swap used by tests.
// It owns no scheduling policy, retry logic or timestamp formatting.
//
// # Allowed dependencies
//
// Layer 0. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - The default clock is held in an atomic pointer; a test that swaps it never races with
//     workers that read the time concurrently.
//   - Production code never calls Swap. Code that can take an explicit Clock (controller,
//     supervisor helpers) should prefer the parameter to the process-wide default.
//   - Swap(nil) installs the real clock, and the function it returns restores the previous
//     clock; the test that installed a clock must defer that restore (t.Cleanup).
//   - Tests that swap the default clock must not use t.Parallel.
package clock
