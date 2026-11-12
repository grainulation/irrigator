// Package providerenv defines the environment boundary for provider processes.
//
// # Responsibility
//
// Claude returns the environment a provider process may inherit: it preserves the
// operating-system context the CLI and its existing home/keychain login need, and drops
// credentials for unrelated services and runtime state.
//
// # Allowed dependencies
//
// Unnumbered: it sits outside the numbered layer table and was not changed by the package
// split. Imports no other internal package. Third-party: none.
//
// # Invariants
//
//   - Provider processes receive this explicit list, never the caller's full environment.
//   - Leaf package: standard library only.
package providerenv
