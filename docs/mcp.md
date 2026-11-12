# MCP server

`irrigator mcp` serves a [Model Context Protocol](https://modelcontextprotocol.io)
server over stdio. Any MCP host (Claude Code, Codex, Cursor, OpenCode) can
inspect Irrigator runs and, when you allow it, drive them. The server calls the
same internal functions as the CLI; it does not shell out to `irrigator` for
status, logs, review, validation, retry or approvals.

```
irrigator mcp [--runs DIR]... [--workspace DIR] [--allow-control]
```

| Flag | Meaning |
| --- | --- |
| `--runs DIR` | A directory whose subdirectories are runs. Repeatable. Tools accept a run name found under these directories. |
| `--workspace DIR` | Git repository used by `review_list`, and the base for relative plan paths. Default: the current directory. |
| `--allow-control` | Also register the tools that change a run. Off by default. |

Every tool that takes a `run` also accepts an explicit run directory path, so
`--runs` is optional. stdout carries the protocol; the server writes nothing else
there.

## Tools

Read-only (always registered):

| Tool | Result |
| --- | --- |
| `list_runs` | Name, path, control state, verified/total cells and reported cost for each run under `--runs`. |
| `run_status` | One run: per-cell status, attempts and errors (truncated to 1 KB). |
| `run_logs` | Sequenced events with `since` and `limit` (default 100, at most 500); same data as `irrigator logs --json`. Continue with the returned `next_since`. |
| `inspect_cell` | One cell's spec, attempts and result fields, as `irrigator inspect`. |
| `review_list` | The review queue for a workspace and optional `base`, as `irrigator review list`. |
| `validate_plan` | Validate a plan `path` or inline `json`; returns `valid` and the errors. An invalid plan is a normal result, not a tool error. |

Control (registered only with `--allow-control`):

| Tool | Result |
| --- | --- |
| `apply` | Create a run from a plan into a new run directory and start its controller detached; returns the run path. Takes `backend`, `budget`, `workers`, `timeout_seconds`, `schedule`. |
| `pause` | Ask the run to drain; durable state is retained. |
| `resume` | Start a detached controller for a stopped run. `retry_failed` retries replay-safe failures only. |
| `retry_cell` | Prepare an explicit retry. `allow_uncertain` and `confirm_stopped` are required arguments. |
| `decide_approval` | Approve or deny a blocked request through the run's mailbox or approval ledger. |

`apply` follows the CLI rules: a per-call `budget` is required (managed plans
use profile budgets and must omit it), a non-managed plan needs an explicit
`backend` (`mock` is free; `haiku` and named providers can spend money), and an
existing run directory is never overwritten. A bare `run` name is created under
the first `--runs` directory. `apply` takes an optional `executor`
(`local`, `herdr` or `tmux`, checked against the registry before anything is
created and recorded for the run); it does not yet take `--sizing` or
`--session-registry`. `list_runs`, `run_status` and `apply` report the run's
`executor`.

## Results

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
