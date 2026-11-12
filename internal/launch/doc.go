// Package launch describes how a task process is configured and launched.
//
// # Responsibility
//
// Execution profiles and catalogs, runtime guardrails and contracts, the sandbox (container
// argv, resource limits), the launch capsule and work context handed to a worker, prompt-input
// allowlists, child-process spawning with bounded output files, and the `_supervise` process
// supervisor (Supervise).
//
// # Allowed dependencies
//
// Layer 8. May import only lower layers; today it imports buildinfo, clock, commandflags,
// jsonio, localfs, model. Third-party: none. The compiler rejects import cycles; the layer
// order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - The supervisor owns the provider process lifetime and a duplicate of the node lease. EOF
//     on its private liveness pipe means the worker has gone away. No durable PID is used for
//     cleanup, so recovery cannot kill a recycled process ID.
//   - Process output is streamed to bounded files, never accumulated without limit in memory.
//   - Sandbox argv never contains environment values.
//   - Context and mount paths must be clean absolute paths and aliases through symlinks are
//     rejected, including for destinations that do not exist yet.
//   - Timing decisions read time through internal/clock so tests move deadlines and waits
//     together.
package launch
