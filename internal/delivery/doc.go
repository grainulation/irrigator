// Package delivery assembles and accepts journaled deliveries.
//
// # Responsibility
//
// `irrigator deliver`: assembly of delivery files from task outputs and journaled acceptance
// (formerly workflow.go).
//
// # Allowed dependencies
//
// Layer 30. May import only lower layers; today it imports artifactstore, checkpoint, graph,
// jsonio, launch, localfs, model, runops. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Disjoint maps are merged without a model integration call; an existing delivery is reused
//     only when its provenance and every source file match exactly.
//   - A journaled acceptance is durable evidence: a transient precondition failure does not
//     erase it, and a changed graph revision refuses to reuse a prior journal.
//   - Competing deliveries are serialized by delivery.lock; delivery.lock does not protect
//     against resume, which the controller lock does.
package delivery
