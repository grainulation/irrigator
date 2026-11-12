// Package provider serves provider calls and reconciles provider cost.
//
// # Responsibility
//
// The provider pool and its durable operation ledger (ServeProvider), provider migration,
// billing registry and reconciliation, and the legacy and projected cost statements.
//
// # Allowed dependencies
//
// Layer 19. May import only lower layers; today it imports artifactstore, checkpoint,
// commandflags, cost, graph, jsonio, launch, localfs, managed, model, observer, providerenv.
// Third-party: none. The compiler rejects import cycles; the layer order is recorded in
// docs/architecture.md.
//
// # Invariants
//
//   - ServeProvider owns the durable operation ledger and a bounded pool of persistent CLI
//     conversations. Spent plus reserved plus the request must fit the configured allocation
//     before a call is admitted.
//   - Startup recovery does not replay unfinished calls.
//   - A reused cumulative cap never grants more spending authority than the new configuration.
//   - Billing evidence must be an unambiguous object (encoding/json alone would accept
//     ambiguous input), and reported integer micros accept only a plain non-negative decimal
//     integer within range.
package provider
