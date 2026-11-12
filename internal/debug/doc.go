// Package debug diagnoses runs read-only and exports redacted replay fixtures.
//
// # Responsibility
//
// `irrigator debug` reads the durable checkpoint once and projects bounded, classified
// evidence; replay-fixture export anonymises a run into a fixture that validation and a
// redaction scan accept.
//
// # Allowed dependencies
//
// Layer 24. May import only lower layers; today it imports checkpoint, commandflags, model,
// observer, session. Third-party: none. The compiler rejects import cycles; the layer order is
// recorded in docs/architecture.md.
//
// # Invariants
//
//   - Debug starts no provider, takes no lock, writes no file, and never prints error text,
//     prompts, results or provider configuration.
//   - A fixture's strings come from a closed vocabulary; export validates, serialises, decodes
//     again and scans for redaction before returning bytes.
//   - Duplicate JSON keys at any depth are rejected when a fixture is decoded.
package debug
