// Package coder runs coding agents (Claude Code, Codex, Antigravity) in git worktrees.
//
// # Responsibility
//
// The hidden `_code-worker` command speaks swarm.worker/v1 for one cell, `irrigator code`
// builds and applies a plan of such cells, `irrigator code cleanup` removes a run's worktrees,
// and CodeFlags / MissingFlags support agent CLI flag drift checks.
//
// # Allowed dependencies
//
// Unnumbered: sits outside the numbered layer table, after model (Layer 7) and before executor
// (Layer 20). May import only lower layers; today it imports commandflags, jsonio, model.
// Third-party: none. The compiler rejects import cycles; the layer order is recorded in
// docs/architecture.md.
//
// # Invariants
//
//   - A timeout, interrupted session, or contradictory/malformed execution evidence returns
//     outcome "uncertain", never "settled" or "not_started".
//   - Standalone pre-session rejection requires unambiguous evidence that no work started
//     (and for agy, validated zero-token usage).
//   - Verification stays bound to the collected commit and branch; any dirty worktree or
//     changed HEAD/branch identity leaves the attempt uncertain.
package coder
