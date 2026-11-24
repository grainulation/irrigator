# MCP server

`irrigator mcp` serves a [Model Context Protocol](https://modelcontextprotocol.io)
server over stdio. Any MCP host (Claude Code, Codex, Cursor, OpenCode) can
inspect Irrigator runs and, when you allow it, drive them. The server calls the
same internal functions as the CLI; it does not shell out to `irrigator` for
status, logs, review, validation, retry or approvals.

```
irrigator mcp [--runs DIR]... [--workspace DIR] [--allow-control] [--canonical-names]
```

| Flag | Meaning |
| --- | --- |
| `--runs DIR` | A directory whose subdirectories are runs. Repeatable. Tools accept a run name found under these directories. |
| `--workspace DIR` | Git repository used by `review_list`, and the base for relative plan paths. Default: the current directory. |
| `--allow-control` | Also register the tools that change a run. Off by default. |
| `--canonical-names` | Select the verb/object naming preview. Legacy names remain the default; this flag changes names, not permissions. |

Every tool that takes a `run` also accepts an explicit run directory path, so
`--runs` is optional. stdout carries the protocol; the server writes nothing else
there.

## Tools

Read-only (always registered; annotated with `readOnlyHint: true`, `destructiveHint: false`, `idempotentHint: true`, `openWorldHint: false`):

| Tool | Result |
| --- | --- |
| `list_runs` | Name, path, control state, verified/total cells and reported cost for each run under `--runs`. |
| `run_status` | One run: per-cell status, attempts, errors (truncated to 1 KB), and `last_activity` (`attempt`, `line`, `truncated`, `updated_at` from the last 4 KiB of `<attempt>.stderr.log`, sanitized and capped at 200 runes) when an attempt stderr log exists. |
| `run_logs` | Sequenced events with `since` and `limit` (default 100, at most 500); same data as `irrigator logs --json`. Continue with the returned `next_since`. |
| `inspect_cell` | One cell's spec, attempts, result fields, and `last_activity` (when `<attempt>.stderr.log` exists), as `irrigator inspect`. |
| `attempt_output` | Bounded pages of one exact attempt's captured stdout/stderr. A source-bound cursor distinguishes unread pages from capture loss; see [attempt output](attempt-output.md). |
| `provider_limit` | Exact-attempt quota/reset advisory from captured output. Conflicting or missing reset evidence stays unknown; no automatic retry or fallback. |
| `delivery_summary` | Recorded task selection, node/check outcomes, output conflicts and candidate identity; never assembles files or runs a checker. |
| `review_list` | The review queue for a workspace and optional `base`, as `irrigator review list`. |
| `inspect_review` | One queued branch's pinned-head diff and metadata, or a queued document's body and comments. Supply exactly one `branch` or `snapshot` ID from the queue; `file` narrows branch diffs. No viewed marks or decisions are recorded. |
| `validate_plan` | Validate a plan `path` or inline `json`; returns `valid` and the errors. An invalid plan is a normal result, not a tool error. |

Control (registered only with `--allow-control`; annotated with `readOnlyHint: false`, `openWorldHint: false`, and per-tool `destructiveHint`/`idempotentHint`):

| Tool | Result |
| --- | --- |
| `apply` | Create a run from a plan into a new run directory and start its controller detached; returns the run path. Takes `backend`, `budget`, `workers`, `timeout_seconds`, `schedule` (`destructiveHint: true`, `idempotentHint: false`). |
| `pause` | Ask the run to drain; durable state is retained (`destructiveHint: false`, `idempotentHint: true`). |
| `resume` | Start a detached controller for a stopped run. `retry_failed` retries replay-safe failures only (`destructiveHint: false`, `idempotentHint: false`). |
| `retry_cell` | Prepare an explicit retry. `allow_uncertain` and `confirm_stopped` are required arguments (`destructiveHint: true`, `idempotentHint: false`). |
| `decide_approval` | Approve or deny a blocked request through the run's mailbox or approval ledger (`destructiveHint: true`, `idempotentHint: false`). |

`apply` follows the CLI rules: a per-call `budget` is required (managed plans
use profile budgets and must omit it), a non-managed plan needs an explicit
`backend` (`mock` is free; `haiku` and named providers can spend money), and an
existing run directory is never overwritten. A bare `run` name is created under
the first `--runs` directory. `apply` takes an optional `executor`
(`local`, `herdr` or `tmux`, checked against the registry before anything is
created and recorded for the run); it does not yet take `--sizing` or
`--session-registry`. `list_runs`, `run_status` and `apply` report the run's
`executor`.

## Naming preview and exact profiles

The default naming contract retains existing tool names. `--canonical-names`
selects one alternate inventory; it does not add aliases. Update host allowlists
and prompts when selecting it. Start a fresh connection after changing startup
arguments. Both naming contracts contain **10 inspection tools**, or **15 tools**
with `--allow-control`. The new `inspect_review` closes the read-only queue-to-content
inspection path. This addition does not expose review decisions or execution controls.

| Default name | Canonical preview |
| --- | --- |
| `list_runs` | `list_runs` |
| `run_status` | `get_run_status` |
| `run_logs` | `list_run_events` |
| `attempt_output` | `get_attempt_output` |
| `provider_limit` | `get_attempt_limit_advisory` |
| `delivery_summary` | `get_delivery_summary` |
| `inspect_cell` | `inspect_cell` |
| `review_list` | `list_reviews` |
| `inspect_review` | `inspect_review` |
| `validate_plan` | `validate_plan` |
| `apply` | `start_run` |
| `pause` | `pause_run` |
| `resume` | `resume_run` |
| `retry_cell` | `retry_cell` |
| `decide_approval` | `decide_approval` |

The inspection profile covers saved-run and review inspection. The control
profile adds run supervision, not Git merging, delivery application or general
host administration. A score for one startup configuration does not establish
quality or completeness of the other. Shipped plugins keep their read-only setup.

All hints are explicit, including closed-world local inspection. Mutating tools
are conservatively destructive: starting a provider can modify its environment,
pause changes dispatch state, retry resets current cell fields, and approval/denial
is a single-use decision. No control is declared idempotent: even repeated pause
requests append events. Hints describe behavior; they grant no authorization.

| Tool | readOnlyHint | destructiveHint | idempotentHint | openWorldHint |
| --- | --- | --- | --- | --- |
| `list_runs` | true | false | true | false |
| `run_status` | true | false | true | false |
| `run_logs` | true | false | true | false |
| `attempt_output` | true | false | true | false |
| `provider_limit` | true | false | true | false |
| `delivery_summary` | true | false | true | false |
| `inspect_cell` | true | false | true | false |
| `review_list` | true | false | true | false |
| `inspect_review` | true | false | true | false |
| `validate_plan` | true | false | true | false |
| `apply` | false | true | false | true |
| `pause` | false | true | false | false |
| `resume` | false | true | false | true |
| `retry_cell` | false | true | false | false |
| `decide_approval` | false | true | false | false |

`validate_plan` may use and remove a private temporary file for inline JSON; it
has no retained effect on the inspected workspace or run. Read-only hints do not
promise identical results while another process changes the files.

Capture a built server's real discovery surface without invoking a tool:

```sh
python3 tools/mcp-schema.py --binary ./irrigator > inspection-tools.json
python3 tools/mcp-schema.py --binary ./irrigator --allow-control --canonical-names > control-tools.json
```

The capture includes the initialized build identity and startup arguments. It is
not an execution test, native host acceptance, or a numeric tool-quality score.

## Results

Each tool advertises its input and output contracts. The SDK validates successful
structured results against the declared output schema. Tool errors use
`isError:true` with error text rather than the success-result schema.

Each tool returns structured JSON and a short text summary. Any single string
is capped at 8 KB; when a value is cut it ends with a `[truncated: …]` note, the
paths are listed under `truncated`, and the summary says so. Tools never return
environment values: `env` lists keep variable names only, and fields named like
credentials (token, secret, password, api key, authorization) are redacted.
Failures are tool errors with a message; the server keeps running.

## Safety model

- **Read-only by default.** Without `--allow-control` the control tools are not
  registered, so a host cannot call them.
- **Control is opt-in per server.** Register a separate, control-enabled server
  entry only for hosts and sessions you trust to change runs.
- **Uncertainty rules are preserved.** `retry_cell` calls the same function as
  `irrigator retry`. `allow_uncertain` and `confirm_stopped` have no defaults and
  keep the CLI meaning: an uncertain outcome is retried only when you attest
  both, and a stopped provider process is never assumed. Interrupted work is
  never replayed automatically, and `resume` only retries replay-safe failures.
- **Approvals use the existing ledger.** `decide_approval` consumes a single-use
  decision through the same code as `irrigator approvals serve`, recorded with
  `via: mcp`. A decision cannot be applied twice.
- **Admitted state is immutable.** The server never edits plans, accepted
  results or attempts directly.
- **No secrets in argv.** The detached controller is started with only run paths
  and numeric limits; it inherits this server's environment like a shell would.
  Provider credentials set for the host are therefore available to runs you
  start.
- **Paths are not confined.** Tools accept any run directory the server's user
  can read; `--runs` is a convenience, not a sandbox.

## Setup

Claude Code:

```
claude mcp add irrigator -- irrigator mcp --runs ~/irrigator-runs
claude mcp add irrigator-control -- irrigator mcp --runs ~/irrigator-runs --allow-control
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.irrigator]
command = "irrigator"
args = ["mcp", "--runs", "/absolute/path/to/irrigator-runs"]
```

Cursor (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "irrigator": {
      "command": "irrigator",
      "args": ["mcp", "--runs", "/absolute/path/to/irrigator-runs"]
    }
  }
}
```

OpenCode (`opencode.json`):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "irrigator": {
      "type": "local",
      "command": ["irrigator", "mcp", "--runs", "/absolute/path/to/irrigator-runs"],
      "enabled": true
    }
  }
}
```

Use absolute paths in files that do not expand `~`. Add `--allow-control` to
the arguments only when you want the host to change runs.

Remote comparison bases may be returned as full `refs/remotes/...` selectors so a local branch or tag cannot change their meaning when passed to `inspect_review`. Full commit IDs and explicit `refs/...` bases retain their object/reference meaning.
