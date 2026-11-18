// Package triage explains why cells of a run are stuck and what to do next.
//
// # Responsibility
//
// `irrigator triage RUN` gathers the evidence a person needs before deciding (whether the
// agent process is stopped, what it spent, and what is in its worktree/branch) and recommends
// one existing command. `--fix` walks through stuck cells interactively at a terminal and runs
// the chosen resolve, retry or cancel only after explicit confirmation.
//
// # Allowed dependencies
//
// Unnumbered: sits outside the numbered layer table, after session (Layer 22) and before cli
// (Layer 32). May import only lower layers; today it imports checkpoint, commandflags, graph,
// model, session. Third-party: github.com/charmbracelet/x/term. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Plain `irrigator triage RUN` (and `--json`) is strictly read-only and modifies no run or
//     worktree files.
//   - `--fix` requires an interactive terminal and refuses to run while a controller holds the
//     run; uncertain work is never replayed automatically, and unconfirmed process exits require
//     the operator to type "stopped" before `--confirm-stopped` is passed.
package triage
