// Package session is the session history store, registry and live session feed.
//
// # Responsibility
//
// Session history storage, the session registry v2 (publish, probe, derive run status,
// retirement), the session feed and the live session registry.
//
// # Allowed dependencies
//
// Layer 22. May import only lower layers; today it imports backup, checkpoint, commandflags,
// graph, jsonio, localfs, model, observer, review, textsafe. Third-party: none. The compiler
// rejects import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Run status is derived only from durable state and lock probes, never from chat text.
//   - The registry reads v1 as a one-entry registry or v2 and never follows symlinks; v2
//     upserts are locked and atomic.
//   - Only a closed vocabulary of State.Events kinds is exposed; event Detail is never exposed.
package session
