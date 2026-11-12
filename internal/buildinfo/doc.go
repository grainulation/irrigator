// Package buildinfo holds the product version string.
//
// # Responsibility
//
// The Version constant reported by `irrigator version`, launch capsules, the MCP server and
// OpenTelemetry resources. It is the single place a release changes the version.
//
// # Allowed dependencies
//
// Layer 2. Imports no other internal package. Third-party: none. The compiler rejects import
// cycles; the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Version is a constant; nothing in this package reads the environment or build flags.
//   - Leaf package: it imports nothing.
package buildinfo
