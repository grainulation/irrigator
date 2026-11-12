// Package cli is the irrigator command line.
//
// # Responsibility
//
// Argument dispatch (Run, Main), the command table and help, the built-in command wrappers and
// the builtinCLI switch. It is the top layer and may import every other internal package;
// nothing imports it except the root main package and integration tests (clitest must never import it:
// its re-exec hook is a func value set by the caller).
//
// # Allowed dependencies
//
// Layer 32. May import only lower layers; today it imports approvals, backup, branchreview,
// buildinfo, campaign, checkpoint, commandflags, composition, dashboard, debug, delivery,
// executor, gitcontract, graph, jsonio, launch, localfs, managed, mcpserver, messaging, model,
// provider, runops, selfupdate, session, sizing, telemetry. Third-party: none. The compiler
// rejects import cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - dispatch.go keeps the `case "graph", "events", "artifacts", "pause", "down":` line
//     verbatim: verification/plugin-validator-regression.py reads it.
//   - main.go (the module root) is `os.Exit(cli.Main(os.Args))` and nothing else.
//   - Help output and command metadata are golden-tested; do not change them without updating
//     verification/golden.
package cli
