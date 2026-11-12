// Package terminalview renders the terminal dashboard and its OSC escapes as pure data.
//
// # Responsibility
//
// The package turns a Snapshot of run state into frame lines, progress and title OSC sequences,
// notifications and hyperlinks, without any dependency on the process environment, a terminal
// or the clock. LinksEnabled and PathLink decide whether file links may be emitted. Callers
// (dashboard, approvals, branchreview) supply data and writers.
//
// # Allowed dependencies
//
// Layer 5. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Output is a function of its inputs: no clock, environment or terminal reads in the
//     rendering functions (the caller passes elapsed time, width and height).
//   - Untrusted text is made visible-safe (Safe, Clip) before it reaches a frame.
//   - Leaf package: it imports only the standard library.
package terminalview
