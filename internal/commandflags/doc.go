// Package commandflags is subcommand flag parsing and command metadata.
//
// # Responsibility
//
// ParseCommandFlags and ParseCommandArguments implement the CLI convention that positional
// arguments may precede flags, and the hidden --_irrigator_flag_metadata probe that returns a
// command's flags without running it. GoLabeled starts a goroutine with pprof labels. These
// primitives stay low because lower command packages call them.
//
// # Allowed dependencies
//
// Layer 6. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Flag parsing never executes the command: the metadata probe returns CommandMetadata as an
//     error value before any side effect.
//   - Leaf package: it imports only the standard library, so any command package may use it
//     without creating a cycle.
package commandflags
