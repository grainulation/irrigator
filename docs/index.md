# Documentation

Start with [your first run](first-run.md): mock workers, your own plan, an optional real provider call, and recovery. The [architecture guide](architecture.md) maps these contracts to their source files.

## Get Started

- [Your first run](first-run.md): guided walkthrough of initialization, planning, execution, and recovery.
- [Platform support](platform-support.md): runtime and terminal boundaries across supported systems.
- [Choosing a tool](alternatives.md): scope comparisons with scripts and workflow frameworks.
- [Linux terminal dashboard](linux.md): build, run, and inspect a local workflow.
- [Windows (WSL 2)](windows-wsl.md): run the Linux build on Windows.
- [Container sandboxes](sandbox.md): containerized execution for command providers (Docker, Podman, Apple Containers).

## Agent Plugins & MCP

- [Agent setup](agent-setup.md): install for Claude Code, Codex, Antigravity or any MCP agent.
- [MCP server](mcp.md): inspect and, when allowed, drive runs from any MCP host.
- [Claude Code plugin](claude-plugin.md): Claude Code plugin manifest and configuration.
- [Codex plugin](codex-plugin.md): Codex CLI and IDE integration.
- [Host integration](host-integration.md): use Markdown skills with a real CLI.
- [Plugin architecture](plugin-architecture.md): plugin layout, roots, and boundaries.
- [Plugin packaging](plugin-packaging.md): source-only plugin layout and validation checks.
- [Launch context](launch-context.md): host discovery and environment isolation.

## Model the Work (Plans & Swarms)

- [Turn shapes](turn-shapes.md): main agents, optional turns, and terminal helpers.
- [Swarm shapes](swarm-shapes.md): A×D×S ownership trees and hierarchy.
- [Workflow composition](workflow-composition.md): pinned reusable graphs and typed artifacts.
- [Execution profiles](execution-profiles.md): providers, models, authority, and budgets.
- [Plan sizing](sizing.md): task graph sizing, estimation, and validation before launch.
- [Swarm entrypoint](swarm-entrypoint.md): execution entrypoints and graph admission.
- [Scheduling order](scheduling-order.md): deterministic graph topological sorting and dependencies.
- [Child normalization](child-normalization.md): canonical checkpoint representation for spawned tasks.
- [Observer shape contract](observer_shape_contract.md): read-only `observer_shape` plan metadata.
- [Generation token limits](token-limits.md): `max_output_tokens` on local Ollama profiles.
- [Public display metadata](public-display-metadata.md): plan-authored display strings.

## Operate & Recover

- [Coding cells](coding-cells.md): `irrigator code` runs Claude Code, Codex or Antigravity in git worktrees with optional OS sandboxing (`--sandbox-verify`), landing as review branches.
- [Local review](local-review.md): the review queue, change review and cleanup.
- [Workflow diagnostics](workflow-diagnostics.md): explicit role routing, provider limit advisories, delivery summaries and the offline mock benchmark.
- [Attempt output](attempt-output.md): bounded, redacted pages of exact historical attempt captures.
- [Recovery](recovery.md): triage stuck cells, explicit retry, exclusive transfer and checkpoint migration.
- [Approvals](approvals.md): phone approve/deny through single-use links.
- [Read-only monitoring](monitoring.md): bounded observations, selected evidence and explicit judgments.
- [Delivery workflow](delivery-workflow.md): acceptance, export, and delivery checks.
- [Debug workflow](debug-workflow.md): record and replay execution fixtures.
- [Executors](executors.md): local, Herdr tmux, and external execution backends.
- [Experiments](experiments.md): opt-in messaging between tasks and swarms, with guardrails.
- [Terminal workspace](terminal-workspace.md): terminal observation and review.
- [Durable mailbox](durable-mailbox.md): bounded messages and receipts.
- [ACP command worker](acp-worker.md): approval-driven coding agents over the Agent Client Protocol.
- [Campaigns](campaigns.md): shared local budget grants.
- [Backup and restore](backup-restore.md): preserve evidence without granting execution authority.
- [Billing reconciliation](billing-reconciliation.md): external attestations and retained uncertainty.
- [Managed history](managed-history.md): explicit accepted history across compatible turns.
- [Local Ollama](managed-ollama.md): explicitly configured local model execution.
- [Provider executable migration](provider-migration.md): review and approve a replacement without rewriting admitted plans or past attempts.

## Sessions

- [Session registry](session-registry-v2.md): session discovery and observation.
- [Live session follow](live-session.md): registry format and the event vocabulary behind `status --watch`.
- [Session demo](session-demo.md): pause, retire and resume transitions with the mock backend.

## Releases & Packaging

- [Homebrew](homebrew.md): stable and development installation, plus pinned release formula updates.
- [CLI runtime archives](runtime-release.md): reproducible local packages and clean-install checks.
- [Local release workflow](local-release.md): building and testing unsigned CLI packages locally.
- [Versioned release process](versioned-release.md): SemVer release checklist and tag stamping.
- [Release pipeline](release-pipeline.md): GoReleaser snapshots and CI release pipeline.
- [Update verification](update-verification.md): cryptographic verification and signature checks.

## Architecture & Standards

- [Architecture](architecture.md): source ownership and runtime boundaries.
- [Hosting architecture](hosting-architecture.md): remote execution boundaries and staging plans.
- [1.x compatibility](compatibility.md): stable interfaces, saved-run upgrades and experimental exclusions.
- [Runtime contracts](runtime-contracts.md): opt-in admission requirements and evidence receipts.
- [Runtime notation](runtime-notation.md): syntax and schemas for state representations.
- [Provider protocol](provider-protocol.md): worker wire format and lifecycle events.
- [Roadmap boundaries](roadmap.md): non-goals and deferred roadmap items.
- [Dependency licenses](license-inventory.md): third-party module licenses.
- [Contributing](../CONTRIBUTING.md): change requirements and local verification.
- [Changelog](../CHANGELOG.md): revision history.
- [Community conduct](../CODE_OF_CONDUCT.md).
- [Security](../SECURITY.md): threat boundary and reporting.

---

Examples live in [examples](../examples). Command-line help is available through `irrigator --help` and machine-readable `irrigator help --json`.
