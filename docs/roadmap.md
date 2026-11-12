# Roadmap boundaries

Current development focuses on reliable local execution and terminal workflows
on macOS and Linux. Windows users run the Linux build in
[WSL 2](windows-wsl.md), checked in CI. Native Windows support is deferred/planned
and needs validation on actual Windows systems.

## Remote executors

The [hosting architecture plan](hosting-architecture.md) prioritizes correctness,
local performance and maintainable adapters. Local checkpoint transactions,
attempt execution and export artifact publication now have explicit tested
boundaries. A hosted coordinator still requires distributed ownership, dispatch
intent transactions, authenticated authority and deployed failure testing.

Keep provider execution separate from run coordination so an external adapter can
submit bounded work to another execution environment. An adapter must preserve
operation identity, idempotency rules, budget reservations, artifact validation,
cancellation acknowledgement, and uncertainty after a disconnected request.

The existing worker protocol and provider profiles define the integration
boundary. Adapters choose their own execution environment; placement must not
turn a timeout into automatic replay or bypass admission and acceptance checks.

## Hosted sharing

Encrypted snapshots and live sharing are deferred. Any future service should be
explicitly opt-in, publish an allowlisted projection, document its key-delivery
and retention trust boundaries, and offer a self-hosted deployment path.
