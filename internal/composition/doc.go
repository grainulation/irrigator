// Package composition compiles typed compositions into plans.
//
// # Responsibility
//
// Composition templates, pins and compilation to admitted plans (`irrigator compose`). Version
// 1 preserves opaque strings; version 2 carries bounded JSON contracts.
//
// # Allowed dependencies
//
// Layer 15. May import only lower layers; today it imports artifactstore, checkpoint,
// commandflags, jsonio, launch, model. Third-party: none. The compiler rejects import cycles;
// the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - CompileComposition returns the plan together with its provenance (origin and pins), and a
//     compiled plan goes through the same admission as any other plan.
//   - Composition input is read with a bound (CompositionRead) and identified by a SHA-256
//     digest of the raw bytes.
package composition
