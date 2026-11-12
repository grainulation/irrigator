// Package artifactstore is immutable, verified artifact storage plus the model-output artifact contract.
//
// # Responsibility
//
// Store publishes new immutable objects and returns only verified bytes; Local binds one
// private directory to one trusted run scope and confines resolution to it. The package also
// owns the artifact reference and plan schema, model-output artifact normalization and file
// maps, and artifact contracts. Run scope is a storage identity, not authentication: a remote
// adapter must authorize it.
//
// # Allowed dependencies
//
// Layer 9. May import only lower layers; today it imports jsonio, model. Third-party: none. The
// compiler rejects import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - An existing object is never overwritten with different bytes; Get returns only bytes
//     whose size and SHA-256 match the reference.
//   - A Put error does not establish that a remote write had no effect; callers reconcile the
//     same reference before retrying.
//   - A reference carries a canonical relative key and a lowercase 64-hex digest; neither a
//     path outside the run nor credentials.
//   - A producer may narrow an artifact contract's byte and item bounds but never widen the
//     consumer's.
package artifactstore
