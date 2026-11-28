// Package admission decides how much new work the controller may start right
// now, from signals about the machine and the run.
//
// # Responsibility
//
// Environment-aware admission control: power and thermal state, network
// reachability and an operator override, composed into one Envelope that the
// controller consults after its dependency, budget and sizing gates and before it
// spawns workers. Every sensor reports a Reading over the same fields; Compose
// merges them strictest-wins (minimum slots, AND of booleans, minimum tier,
// maximum poll interval) and records which sensor bound each field. A Smoother
// then applies asymmetric hysteresis: a limit tightens at once and relaxes only
// after a dwell, one slot at a time. Local work (mock, Ollama) and remote work
// (the hosted CLIs) have separate slot limits, so a hot GPU does not starve
// remote tasks and an offline link does not starve local ones.
//
// # Allowed dependencies
//
// Layer 17a. May import only lower layers; today it imports checkpoint, clock,
// jsonio, model. Third-party: none. The compiler rejects import cycles; the layer
// order is recorded in docs/architecture.md.
//
// # Invariants
//
//   - It only decides which already-ready tasks may start now. It never reads or
//     writes the checkpoint beyond looking up a task's bound adapter, never
//     changes a task's binding, and never gates a commit, heartbeat,
//     cancellation or status query.
//   - Configuration and live status are sidecar files (admission.json,
//     admission-status.json, admission-override.json); the plan hash and the
//     state schema are untouched.
//   - A run without --admission has no evaluator at all: behavior is exactly the
//     previous behavior.
//   - Sensors never run on the controller's critical path after the first
//     evaluation, and every duration comes from the injected clock.
//   - Local operating-system probes fail open (a broken probe never stops work and
//     is shown as degraded); hard safety probes fail closed.
//   - A stateful sensor is told the effective envelope so it can clamp its state
//     to it (anti-windup).
package admission
