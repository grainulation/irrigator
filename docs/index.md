# Documentation

Start with [your first run](first-run.md): mock workers, your own plan, an
optional real provider call, and recovery. Then choose the part of the system you need. The [architecture guide](architecture.md) maps these
contracts to their source files.

## Get started

- [Choosing a tool](alternatives.md): scope comparisons with scripts and workflow frameworks.
- [Platform support](platform-support.md): runtime and terminal boundaries.
- [Linux terminal dashboard](linux.md): build, run, and inspect a local workflow.
- [Windows (WSL 2)](windows-wsl.md): run the Linux build on Windows.
- [Agent setup](agent-setup.md): install for Claude Code, Codex, Antigravity or any MCP agent.
- [Host integration](host-integration.md): use Markdown skills with a real CLI.
- [MCP server](mcp.md): inspect and, when allowed, drive runs from any MCP host.
- [Plugin packaging](plugin-packaging.md): source-only plugin layout and checks.
- [CLI runtime archives](runtime-release.md): reproducible local packages and clean-install checks.

## Model the work

- [Turn shapes](turn-shapes.md): main agents, optional turns, and terminal helpers.
- [Swarm shapes](swarm-shapes.md): A×D×S ownership trees.
- [Workflow composition](workflow-composition.md): pinned reusable graphs and typed artifacts.
- [Execution profiles](execution-profiles.md): providers, models, authority, and budgets.
- [Managed history](managed-history.md): explicit accepted history across compatible turns.
- [Public display metadata](public-display-metadata.md): plan-authored display strings.
- [Generation token limits](token-limits.md): `max_output_tokens` on local Ollama profiles.
- [Observer shape contract](observer_shape_contract.md): read-only `observer_shape` plan metadata.
- [Runtime contracts](runtime-contracts.md): opt-in admission requirements and evidence receipts.

## Operate and recover

- [Delivery workflow](delivery-workflow.md): acceptance, export, and delivery checks.
- [Read-only monitoring](monitoring.md): bounded observations, selected evidence and explicit judgments.
- [Coding cells](coding-cells.md): `irrigator code` runs Claude Code, Codex or Antigravity in git worktrees, landing as review branches.
- [Recovery](recovery.md): exclusive transfer and checkpoint migration.
- [Provider executable migration](provider-migration.md): review and approve a replacement without rewriting admitted plans or past attempts.
- [Backup and restore](backup-restore.md): preserve evidence without granting execution authority.
- [Billing reconciliation](billing-reconciliation.md): external attestations and retained uncertainty.
- [Campaigns](campaigns.md): shared local budget grants.
- [Experiments](experiments.md): opt-in messaging between tasks and swarms, with its guardrails.
- [Local review](local-review.md): the review queue, change review and cleanup.
- [Terminal workspace](terminal-workspace.md): terminal observation and review.
- [Durable mailbox](durable-mailbox.md): bounded messages and receipts.
- [Approvals](approvals.md): phone approve/deny through single-use links.
- [Local Ollama](managed-ollama.md): explicitly configured local model execution.
- [Session registry](session-registry-v2.md): session discovery and observation.
- [Session demo](session-demo.md): pause, retire and resume transitions with the mock backend.
- [Live session follow](live-session.md): registry format and the event vocabulary behind `status --watch`.
- [ACP command worker](acp-worker.md): approval-driven coding agents over the Agent Client Protocol.

## Maintain the project

- [Architecture](architecture.md): source ownership and runtime boundaries.
- [Contributing](../CONTRIBUTING.md): change requirements and local verification.
- [Changelog](../CHANGELOG.md) and [roadmap boundaries](roadmap.md).
- [Community conduct](../CODE_OF_CONDUCT.md).
- [Dependency licenses](license-inventory.md): third-party module licenses.
- [Security](../SECURITY.md): threat boundary and reporting.

Examples live in [examples](../examples). Command-line help is available through
`irrigator --help` and machine-readable `irrigator help --json`; versioned plan and worker contracts remain authoritative when
reading older examples or documentation.
