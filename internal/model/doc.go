// Package model defines the shared vocabulary of a run.
//
// # Responsibility
//
// Plan, Task, TaskState, State, Event, the worker wire protocol (WorkRequest, WorkResponse),
// outcome classification, usage, display metadata, artifact contracts and the plain data types
// of every domain that State and Plan embed. It also holds pure helpers over those types (call
// limits, shape arithmetic, adaptive policy, observer and recovery contracts). It performs no
// file or process I/O.
//
// # Allowed dependencies
//
// Layer 7. May import only lower layers; today it imports cost, jsonio. Third-party: none. The
// compiler rejects import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Persisted and wire field names are fixed by JSON tags; a tagged or reflect-walked field
//     is never renamed or removed without a schema or protocol version change.
//   - Pure data and pure functions only: no file, network or process access.
//   - Only cost and jsonio sit below this package, so every other domain package can depend on
//     it without a cycle.
package model
