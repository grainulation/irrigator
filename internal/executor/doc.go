// Package executor runs one task attempt on a backend.
//
// # Responsibility
//
// The local, tmux and herdr executors, the executor registry, the agents probe, cleanup,
// diagnostics, and the worker attempt (request construction, external finalize). Cell
// transitions are reducers over the checkpoint that perform no I/O and take their event time
// explicitly.
//
// # Allowed dependencies
//
// Layer 20. May import only lower layers; today it imports artifactstore, checkpoint, clock,
// commandflags, cost, graph, jsonio, launch, managed, messaging, model, provider, providerenv,
// terminalview. Third-party: none. The compiler rejects import cycles; the layer order is
// recorded in docs/architecture.md.
//
// # Invariants
//
//   - BeginAttempt requires the caller to hold the cell's process lease. A recovered running
//     row is eligible only after the previous lease owner is gone and its provider is
//     replay-safe; a free lease alone does not authorize repeating external work.
//   - Cell-transition reducers mutate only the in-memory state they are given; persistence
//     happens through checkpoint transactions.
//   - Timing reads internal/clock.
//   - Agent discovery never logs in and bounds every probe with a timeout.
package executor
