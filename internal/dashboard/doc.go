// Package dashboard is the terminal dashboard and status presentation.
//
// # Responsibility
//
// The terminal UI (frames, title, progress, notifications), controller display, status executor
// and status watch.
//
// # Allowed dependencies
//
// Layer 28. May import only lower layers; today it imports executor, graph, model, provider,
// session, terminalview, textsafe. Third-party: none. The compiler rejects import cycles; the
// layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Presentation only: it never changes scheduling or durable state.
//   - Status watch probes the controller's process-held lease, not a persisted PID.
//   - Cell notifications fire once when a cell newly needs a person.
package dashboard
