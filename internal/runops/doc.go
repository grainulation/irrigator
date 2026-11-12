// Package runops coordinates run operations: controller, scheduling, cancel/retry/resolve, inspect.
//
// # Responsibility
//
// The controller loop (reconcile, run, drain), scheduling order, cancel, retry and resolve,
// inspect, export and garbage collection. This is where the lower packages are combined into
// commands that act on a run.
//
// # Allowed dependencies
//
// Layer 29. May import only lower layers; today it imports approvals, artifactstore,
// checkpoint, clock, commandflags, dashboard, executor, graph, jsonio, launch, localfs,
// managed, messaging, model, provider, session, sizing. Third-party: none. The compiler rejects
// import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - A controller signals only process handles it created. Persisted worker and provider PIDs
//     remain diagnostic evidence, never cleanup targets.
//   - Scheduling orders only already dependency-ready work and only permutes it.
//   - GC lists run-scoped obsolete attempt diagnostics only and proves the target is a run with
//     a readable checkpoint first.
//   - Drain deadlines are measured with internal/clock.
package runops
