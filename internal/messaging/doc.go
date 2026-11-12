// Package messaging carries messages between tasks: mailbox and experiment channels.
//
// # Responsibility
//
// The durable bounded mailbox between tasks and the experiment channels and notes (publish,
// inbox, approval gate, frozen notes context).
//
// # Allowed dependencies
//
// Layer 13. May import only lower layers; today it imports checkpoint, commandflags, jsonio,
// launch, localfs, model, review, textsafe. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Publication never replaces an existing publication; a crash before the final link leaves
//     only an ignored temporary file, so the next pass retries.
//   - A channel item is opened to subscribers only by the human approval step.
//   - Notes are rendered as quoted data with provenance, never as instructions.
package messaging
