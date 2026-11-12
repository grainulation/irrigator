// Package checkpoint owns the run directory: lock, read, validate, update and recovery gates.
//
// # Responsibility
//
// The run directory is the durable truth about a run. This package takes the run lock
// (FileLock), reads and validates the persisted state, publishes updates through a transaction
// (a run-scoped checkpoint store), loads and admits plans, and holds every persisted-state
// validator, grouped by domain in files named after the domain (adaptive.go,
// campaign_contract.go, experiments.go, graph.go, managed_state.go, observer_shape.go,
// sizing_shape.go, revision.go, turn_runtime.go and others). It also holds the quarantine,
// recovery and campaign guards that gate every write, and setControl.
//
// # Allowed dependencies
//
// Layer 10. May import only lower layers; today it imports artifactstore, commandflags, cost,
// jsonio, launch, localfs, model, store. Third-party: none. The compiler rejects import cycles;
// the layer order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - All processes cooperate through flock; atomic replacement lets readers see either the
//     previous complete checkpoint or the next complete one.
//   - Update serializes admission, invokes its callback at most once on a private snapshot and
//     durably publishes only a successful callback. A callback error aborts publication; a
//     publication error can be ambiguous (rename may have succeeded before the directory fsync
//     failed), so callers reconcile durable operation identity instead of replaying the
//     callback or a provider call.
//   - Nothing retries a callback implicitly on conflict.
//   - RejectRecoveryRun and RejectQuarantinedRun are called at every mutation and dispatch
//     gate; a copied checkpoint has no authority at a different filesystem identity or path.
//     Read-only tools may still inspect a quarantined run.
//   - Observer-shape validation runs only at initial plan load and initialize, never from the
//     generic validation path, so later graph revisions are not vetoed by observer metadata.
//   - Only campaign admission may load an unbound channel plan (LoadCampaignPlan); every other
//     caller keeps the ordinary campaign-binding requirement.
//   - Validators are called explicitly; there is no init() registry.
package checkpoint
