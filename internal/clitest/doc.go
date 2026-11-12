// Package clitest is the process harness for tests that re-execute their own test binary.
//
// # Responsibility
//
// Tests that spawn `_supervise`, `resume`, `_worker` or fixture subcommands re-execute the test
// binary; ReexecMain serves those commands from a test main, and Start/Finished run and await a
// subprocess. The CLI and Fixtures hooks are func values set by the caller.
//
// # Allowed dependencies
//
// Unnumbered: it sits outside the numbered layer table and was not changed by the package
// split. May import only lower layers; today it imports checkpoint, launch. Third-party: none.
//
// Test-only package: production code must not import it.
//
// # Invariants
//
//   - Import it only from _test.go files.
//   - It must never import internal/cli; the caller passes cli.Main through Hooks.CLI.
//   - A re-exec command nobody serves exits 97 instead of re-running the suite recursively.
package clitest
