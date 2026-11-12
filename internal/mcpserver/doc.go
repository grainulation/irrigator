// Package mcpserver is the stdio MCP server over run state.
//
// # Responsibility
//
// `irrigator mcp` exposes runs read-only by default; the tools that change runs (apply, pause,
// resume, retry_cell, decide_approval) are registered only with --allow-control.
//
// # Allowed dependencies
//
// Layer 31. May import only lower layers; today it imports approvals, branchreview, buildinfo,
// checkpoint, commandflags, dashboard, executor, launch, model, provider, runops, session.
// Third-party: github.com/modelcontextprotocol/go-sdk/mcp. The compiler rejects import cycles;
// the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - Tool output is sanitized: values round-trip through JSON and every string is capped on a
//     rune boundary.
//   - A run is resolved from a directory path or a bare name under a --runs root and opened
//     through the same checkpoint read as the CLI.
//   - A control tool fails when a controller already holds the run.
package mcpserver
