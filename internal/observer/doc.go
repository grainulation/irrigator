// Package observer projects run state into observer views.
//
// # Responsibility
//
// Plan observers: shape (declared against the actual active graph), lifecycle (one entry per
// active task) and budget attribution, including the provider ceiling record kept next to the
// provider ledger.
//
// # Allowed dependencies
//
// Layer 16. May import only lower layers; today it imports checkpoint, jsonio, model.
// Third-party: none. The compiler rejects import cycles; the layer order is recorded in
// docs/architecture.md.
//
// # Invariants
//
//   - Projections are read-only over durable state and never infer a shape or guess capacity on
//     a mismatch.
//   - Malformed persisted state never panics a projection.
//   - Capacity is a shared allocation ceiling, not a per-parent promise.
//   - Budget files are read with a size cap and without following symlinks.
package observer
