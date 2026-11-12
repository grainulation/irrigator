// Package graph grows the task graph: expansion, turns, revisions and delivery status.
//
// # Responsibility
//
// Task-graph expansion and ownership, turn runtime and helpers, child-proposal normalization,
// helper admission, revision proposals, the adaptive policy and its decisions, and the
// read-only delivery-status projection.
//
// # Allowed dependencies
//
// Layer 12. May import only lower layers; today it imports artifactstore, checkpoint,
// commandflags, cost, jsonio, launch, managed, model. Third-party: none. The compiler rejects
// import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - A revision replaces the active task snapshot only through the supported proposal
//     mechanism, never by editing the admitted plan.
//   - Child normalization is bounded, deterministic and derived-only: the original result
//     bytes, expansion hash and costs are never touched.
//   - Adaptive admission is estimate-based only; provider reservations remain the actual
//     spending limit.
//   - The delivery-status helper is read-only: it takes no lock beyond a brief shared probe of
//     delivery.lock, prints only a small closed set of values, and reads regular files without
//     following a final symlink.
package graph
