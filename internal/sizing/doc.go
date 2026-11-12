// Package sizing analyses plan shape and gates admission by it.
//
// # Responsibility
//
// Plan sizing: shape arithmetic (A root agents, depth D, S children), deterministic list
// scheduling, the sizing report and the controller admission gate.
//
// # Allowed dependencies
//
// Layer 17. May import only lower layers; today it imports checkpoint, cost, jsonio, model.
// Third-party: none. The compiler rejects import cycles; the layer order is recorded in
// docs/architecture.md.
//
// # Invariants
//
//   - The sizing gate only lowers the number of slots; it never admits more than the runtime
//     would.
//   - Reports are deterministic functions of the state; the report key fingerprints everything
//     the report depends on.
//   - Shape bounds are overflow-checked at every step.
package sizing
