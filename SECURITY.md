# Security

Irrigator is a pre-release local execution tool. This document states what the runtime does and does not protect against, and how to report a problem. It is not a certification and not a promise of support.

## Supported versions

Security fixes target the current maintained branch and release candidate. There is no hosted service or guaranteed response-time commitment.

## Reporting a vulnerability

Do not post sensitive vulnerability details in a public issue. Use private reporting
from the repository's [Security tab](https://github.com/grainulation/irrigator/security)
when available. Otherwise ask a maintainer for a private reporting channel without
including exploit details or credentials. No bounty program is implied.

Include:

- the version or source identity (commit, or file hashes);
- the command, plan, and provider configuration that reproduce it, with secrets removed;
- what you expected, what happened, and the impact you believe it has;
- whether any paid provider call was involved, and its reported cost.

## Trust model

Irrigator is a local, single-host runtime for trusted plans and trusted providers.

- **Cells are not sandboxes.** A cell is a local directory. Providers and command validators are trusted local processes running with the invoking user's permissions. Do not run plans or providers you do not trust.
- **Process boundary.** Providers use direct argument arrays, never a shell. Use absolute executable paths. Custom providers receive PATH, HOME, TMPDIR, TEMP, and SystemRoot plus explicitly named plan `env` variables. Built-in and managed Claude processes receive only PATH, HOME, USER, TMPDIR, TEMP, and SystemRoot, preserving their home/keychain login without inheriting shell API keys or runtime control tokens. Codex has its own adapter-specific allowlist. These are environment filters, not filesystem sandboxes.
- **Limits.** Provider inputs are capped at 1 MiB, stdout at 4 MiB, and retained stderr at 1 MiB.
- **Built-in Haiku backend.** It uses structured output, safe mode, no built-in tools, an empty MCP configuration, and no CLI conversation persistence. `doctor` distinguishes finding the Claude CLI from authentication; finding it does not verify login.
- **Input selection is not isolation.** `input_fields` and `context` reduce what a task receives. They are not a capability restriction or sandbox.
- **Delivery checks.** Acceptance runs a command array in the delivery directory and must not modify delivered source. A structurally valid model answer is not proof that its claims or code are correct; review generated code before integrating it.
- **Recovery.** Recovery never kills a stored PID. A node whose provider shutdown is unconfirmed becomes `uncertain` and blocks automatic replay. Inspect and stop surviving processes yourself, then use `resolve ... --confirm-stopped` (an operator attestation) or `retry ... --allow-uncertain`. The runtime cannot guarantee exactly-once external effects or billing. Interrupted calls can incur charges that are missing from reported totals.

## Network destinations and credentials

Irrigator makes outbound requests only to destinations the operator configures, and sends a credential only to the destination it was configured for.

- **Destinations are operator-chosen.** The OTLP collector (`--endpoint`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`), the ntfy server (`IRRIGATOR_NTFY_SERVER`) and the notification webhook are set by whoever runs the tool. There is no built-in host allowlist: anyone who can set those variables in the invoking environment already runs code as that user, so a collector or server on your own infrastructure is a supported choice.
- **Rules applied to every credentialed destination.** `http` or `https` only, with plain `http` accepted for loopback only (`OTEL_EXPORTER_OTLP_INSECURE` is refused for any other collector). URLs may not carry credentials (userinfo), and an OTLP endpoint may not carry a query or fragment. Credentials come from the environment (`OTEL_EXPORTER_OTLP_HEADERS`, `IRRIGATOR_NTFY_TOKEN`), never from a URL, and are read only after the destination has passed these checks. (Credentials a provider needs, such as the `serve` connection file, are local and unrelated to outbound requests.)
- **No redirects.** The OTLP exporter and the notifier return a redirect response as it is, and the ACP worker's fallback notifier treats a redirect as an error, so headers and approval links are never forwarded to another host.
- **Error text.** Notification failures are reported as fixed text or a numeric status and do not echo server-supplied reason phrases or bodies, URLs or credential values. OTLP export errors, including those the SDK logs from its background batch processor, have header values and URL paths redacted and control characters made visible before they leave the exporter; collector-supplied text is otherwise kept so a failure stays diagnosable. Provider-reported error text has forwarded credential values (explicit plan `env` variables, 6 characters or longer, also when base64-, JSON- or percent-encoded in either hex case) replaced with `[redacted]`. Malformed `OTEL_EXPORTER_OTLP_*HEADERS` values are rejected without being echoed.
- **Raw provider output is not scrubbed.** The per-attempt `N.stdout.json` and `N.stderr.log` files keep what the provider returned. A provider that reflects a credential it was given will therefore have it in those files. They are covered by the run-data guidance below; give providers only the credentials they need.
- **Terminal output.** Agent- and server-controlled text is rendered with control, format and bidirectional characters made inert (shown as visible markers) before it reaches a terminal, including OTLP collector responses and the ACP worker's progress and fatal-error output.

## Local data handling

- Run directories keep `state.json`, raw inputs and responses, logs, and artifacts on local disk. `gc --apply` removes only eligible obsolete per-attempt diagnostic files (`N.input.json`, `N.stdout.json`, `N.stderr.log`, including check variants) inside a valid, quiescent run, so those raw attempt inputs, outputs and error logs are not retained indefinitely; it keeps the most recent attempts, files protected by uncertain or unsettled execution state or by revision history, checkpoints, accepted artifacts and other run data, and it never reclaims Git worktrees or branches. Run data may contain prompts, source, and model output. Treat it as sensitive and do not commit it; delete or archive it yourself when you no longer need it.
- Agent worktrees and branches created by coding cells persist on disk with any uncommitted content until you remove them explicitly. They may hold source and secrets the agent touched.
- `serve` writes `connection.json` in its directory. Keep it private; it authenticates connectors to the loopback service. The service listens only on loopback.
- Reused provider sessions keep earlier turns in the conversation. This is bounded reuse, not isolation between unrelated tasks. Do not pool tasks that must not see each other's context.
- The current terminal surfaces (`review`, `session`) and the `mcp` server read local run, session and workspace data. Session feeds in `.irrigator/sessions` keep agent prompts, one-line tool activity and replies, and review output may show task text, so treat them as sensitive, keep them out of version control, and do not share terminal output that contains private task text. They are not a boundary against processes running with your permissions.
- Public plan metadata (titles, why, done-when, outcome) is displayed only when the plan opts in. Do not put secrets in it.

## Spending safety

New managed plans use best-effort `track` accounting: unknown cost does not hold
funds or block otherwise valid work, and run thresholds only warn. Existing
enforced plans keep their admitted limits. Tracking does not override explicit
provider-service budgets, campaign grants, or provider-account limits. Execution
uncertainty and process-cleanup requirements still prevent unsafe replay.
Amounts labelled USD are provider estimates unless separately reconciled with
billing evidence; no currency conversion or token pricing is inferred.

CLI `--call-budget` (`--budget` alias) limits are per call. Unmanaged apply,
prepare and deliver require explicit backend selection; managed plans use their
declared execution profiles. Interrupted built-in paid calls are not replayed
automatically. Available partial cost reports are retained and ambiguous
charges stay visible through durable unknown-cost events.

When a provider receipt cannot be saved, the affected operation reports an error
and remains uncertain; it is never automatically replayed. The service retains
the greater of its reservation and observed cost, then attempts to persist the
repaired ledger before admitting unrelated work within the remaining budget.
On restart, matching durable cost metrics preserve observed charges. Missing
metrics retain the full reservation with a warning that the actual charge is
unknown and may exceed it. This permits continued work with explicit accounting
uncertainty; it does not guarantee a provider invoice ceiling.

Unwritable accounting storage, invalid existing cost evidence, or exhausted
allowance still prevents new paid dispatch. Cached completed responses remain
readable. Preserve the ledger and reconcile unknown external charges; deleting
it or choosing another service directory does not resolve the old exposure.
Service recovery is separate from managed run billing reconciliation below.

Managed execution profiles add durable
run-level reservations, and campaigns reserve allowances across participating
runs. These are admission controls, not guarantees about a provider invoice.
Unknown costs and uncertain executions retain exposure. External billing
attestations are separate from provider reports, deduplicated within the configured
registry, and never resolve uncertain execution by themselves. Do not run paid
work without an explicitly authorized budget. See [campaigns](docs/campaigns.md)
and [billing reconciliation](docs/billing-reconciliation.md).

## Plugins and skills

The plugin and skill files are Markdown and JSON instructions for a host agent that drives the local `irrigator` CLI. They do not add an MCP server, grant host permissions, or send messages. Installing a plugin into a host is a local action the user takes under that host's own permission model; this repository does not install anything automatically. The packaging step is local only and does not publish. Review any skill before enabling it, because a skill steers an agent that can run commands.

## Out of scope

The runtime is a bounded single-host tool. A hosted control plane, remote provisioning, multi-tenant isolation, and a sandbox for untrusted code are not implemented. Reports that depend on running untrusted plans, providers, or validators as if they were isolated are design limitations; describe them as hardening requests rather than vulnerabilities.

## Hardening checklist for operators

- Run only plans and providers you reviewed.
- Keep run directories and `connection.json` readable only by you.
- Keep secrets out of plans and context; allowlist only the environment variables a provider needs.
- Authorize a paid budget explicitly and prefer `--backend mock` while testing.
- Do not publish review, session or MCP output, or metadata, that contains private task text.
