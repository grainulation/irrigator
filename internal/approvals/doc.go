// Package approvals is the human-approval surface and run notifications.
//
// # Responsibility
//
// The approval server and link minting (token consumption, per-run key), exposure through
// tailscale or cloudflare, notifiers (ntfy, webhook) and run notifications.
//
// # Allowed dependencies
//
// Layer 21. May import only lower layers; today it imports checkpoint, clock, commandflags,
// jsonio, messaging, model, provider, terminalview. Third-party: none. The compiler rejects
// import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - An approval link is single-use: the token is consumed first and then the decision is
//     recorded, so one link can never produce two decisions.
//   - The per-run key is created exclusively with mode 0600 and a weak or loose key is refused.
//   - The listener binds only a dialable loopback address.
//   - Decisions are serialized across goroutines and processes with a lock, and each record is
//     one O_APPEND write.
package approvals
