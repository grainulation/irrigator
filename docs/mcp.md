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
| `--workspace DIR` | Git repository used by review tools, and the base for relative run, workspace and plan paths. Default: the current directory. |
| `--allow-control` | Also register the tools that change a run. Off by default. |
| `--canonical-names` | Select the verb/object naming preview. Legacy names remain the default; this flag changes names, not permissions. |

Every tool that takes a `run` also accepts an explicit run directory path, so
`--runs` is optional. stdout carries the protocol; the server writes nothing else
there.

Startup `--runs` and `--workspace` paths resolve against the server process's
working directory. Relative `--runs` roots do not resolve against `--workspace`.
Prefer absolute paths in host configuration. Omitting `--runs` gives an empty
discovery list; explicitly passing `--runs ""` instead selects the process's
working directory, just like `--runs .`. Repeated roots retain their order, and
missing or unreadable roots appear as discovery error rows.

### Run selectors and task identifiers

For an existing run, a bare name such as `r` searches the configured `--runs`
directories in order and selects the first matching directory. With configured
roots, a missing bare name is an error. A filesystem error while inspecting a
candidate stops lookup instead of silently selecting a later run. Use `./r` to select a run relative to
`--workspace`, or an absolute path to select it directly. Without `--runs`, a bare
name also resolves relative to `--workspace`. Creating a new run with `apply`
places a bare name under the first configured root. The selectors `.` and `..`
always resolve relative to `--workspace`, including for creation.

Use a successful `list_runs` row's `path`, or the resolved `path` from
`run_status`, to select that exact run again. A bare name follows root order and
can select a different row when multiple roots contain the same name. Diagnostic
`run_id` fields identify saved runs; they are not directory selectors.
Discovery reports paths that cannot be represented as UTF-8 as diagnostic rows
without `path` or `name` selectors; the error quotes the original path bytes.
Configuration and resolved run destinations must be representable as UTF-8;
inspection and control reject unrepresentable paths before accessing run state.

The `id` in `run_status.cells[]` is the task identifier accepted as
`inspect_cell.cell`, `run_logs.cell`, `attempt_output.task`, and
`provider_limit.node`. Keep that ID when moving between tools. The `cell` returned
by `inspect_cell` is a directory path, while its `node` is a state object.
Historical-output tools also require an explicit recorded attempt number;
an attempt count does not establish that every attempt has captured output.

When passing a queue item from `review_list` to `inspect_review`, repeat the
selected `workspace`, including when it differs from the server default. For a
branch, also pass `queue.base`; for a document, pass `snapshot` and omit
`branch`, `base` and `file`.

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

Control (registered only with `--allow-control`; all declare `readOnlyHint: false`,
`destructiveHint: true` and `idempotentHint: false`; `apply` and `resume` also declare
`openWorldHint: true` because providers can contact external services):

| Tool | Result |
| --- | --- |
| `apply` | Create a run from a plan into a new run directory and start its controller detached; returns the run path. Takes `backend`, `budget`, `workers`, `timeout_seconds`, `schedule` (`destructiveHint: true`, `idempotentHint: false`). |
| `pause` | Ask the run to drain; durable state is retained. Active providers can continue, and each repeated request appends an event. |
| `resume` | Start a detached controller for a stopped run. `retry_failed` retries replay-safe failures only. |
| `retry_cell` | Prepare an explicit retry. `allow_uncertain` and `confirm_stopped` are required arguments (`destructiveHint: true`, `idempotentHint: false`). |
| `decide_approval` | Approve or deny a blocked request through the run's mailbox or approval ledger (`destructiveHint: true`, `idempotentHint: false`). |

## Following a pause and completing local workflows

`pause` saves a drain request and returns immediately. `run_status.control=pause`
means the request was recorded; active provider calls can still be running.
Watch cell progress with `run_status` and use `run_logs` for transitions.
Drain time depends on the active calls. `resume` checks for an existing controller
before launching; `started:true` acknowledges process creation, not controller
ownership or subsequent progress. A concurrent resume or early child exit can
prevent that child from running the controller. Check `run_status` and `run_logs`
for subsequent progress. A checkpoint or free controller lock alone cannot
establish that an interrupted provider process stopped or had no external effects.
Retry and resolution retain their existing checks; neither is proof that external
effects were absent. Legacy attempts can lack process markers, so inspect cleanup
evidence explicitly before reconciling an uncertain result.

MCP covers inspection and optional run supervision. The local CLI also supports
operations that require a terminal workflow:

| Intent | Local CLI handoff |
| --- | --- |
| Request termination of run compute | `irrigator down RUN` |
| Request cancellation of a cell and its descendants | `irrigator cancel RUN CELL` |
| Reconcile an uncertain result | `irrigator resolve RUN CELL --result FILE` |
| Record a branch review verdict | `irrigator review approve --workspace REPO --base BASE BRANCH` or `irrigator review request-changes --workspace REPO --base BASE BRANCH --body TEXT` |
| Record a document review verdict | `irrigator review approve --workspace REPO --snapshot SNAPSHOT_DIR` or `irrigator review request-changes --workspace REPO --snapshot SNAPSHOT_DIR --body TEXT` |
| Preview review storage cleanup | `irrigator review prune --workspace REPO` |
| Recompute and apply eligible storage cleanup | `irrigator review prune --workspace REPO --apply` |

These are separate local commands, available when the host has terminal access.
For a CLI document verdict, use the absolute directory in
`queue.documents[].path`. The `snapshot` ID is the selector for MCP
`inspect_review`; it is not a CLI directory. `--workspace` selects the
review ledger, while a relative CLI snapshot directory follows the shell's cwd.
Use the host's normal permissions and the resolved absolute run path from
inspection; shell lookup of a bare run name can select a different directory.
A stop request does not establish process cleanup or undo external effects; see
[recovery](recovery.md) before replaying uncertain work. Branch verdicts resolve
the branch again and record its head at verdict time; compare that head with the
one inspected and review any movement before relying on the verdict. Verdicts
do not merge branches. The `storage` field in
`review_list` previews cleanup with the default 30-day retention; pruning uses
the selected workspace. Each prune call recomputes eligibility; a previous preview
does not freeze the deletion list. Storage ancestors must be real directories,
and inconclusive subject lookups stop pruning rather than proving an orphan.
An unavailable storage preview is returned as `storage.error` without hiding
an otherwise readable review queue. Explicit pruning still fails closed.
See [local review](local-review.md) for review and cleanup
details. The canonical names `pause_run`, `get_run_status`, `list_run_events`
and `resume_run` have the same lifecycle as their default counterparts.

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

Select and evaluate each profile independently:

| Startup arguments | Names | Tools | Scope |
| --- | --- | --- | --- |
| `mcp` | Legacy | 10 | Saved-run and workspace review inspection |
| `mcp --canonical-names` | Canonical preview | 10 | The same inspection operations |
| `mcp --allow-control` | Legacy | 15 | Inspection and explicit run controls |
| `mcp --allow-control --canonical-names` | Canonical preview | 15 | The same inspection and control operations |

For a host adopting canonical names, update that host's startup arguments,
tool allowlists and prompts, then reconnect and inspect its actual `tools/list`
response. A registry or another host runs its own configuration: changing a local
connection does not select its inventory. Record the startup arguments, build
identity and captured definitions when comparing external evaluations. A schema
capture or a local assessment does not establish a new published score.

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

Each tool advertises its input and output contracts. Successful structured
results are validated against the declared output schema using a numeric
validation copy; the wire JSON retains the original exact numeric values.
Clients need integer-preserving JSON decoding to retain that precision.
The SDK validates inputs. Tool errors use
`isError:true` with error text rather than the success-result schema.

Successful calls return structured JSON and a short text summary. Their displayed
string values and summaries are capped at 8 KiB, including any truncation notice, on a
UTF-8 boundary. Run-status errors and run-list checkpoint errors have a lower
1 KiB cap. Truncated structured values end with a `[truncated: …]` note; their
paths are listed under `truncated`, and the summary discloses the shortening.
Summaries that need shortening carry their own inline notice; `truncated` paths
refer to structured values. These generated JSON-path metadata strings retain
their exact addresses and are exempt from the display-value cap. Simple property
names use dot notation; other names use quoted bracket selectors.
In structured results, `env` entries retain variable names only.
Credential-named values, including arrays and objects, are redacted; ordinary
numeric token-usage measurements retain their exact numeric values. The public
`cost.tokens` accounting projection is preserved; credential-looking containers
in opaque event data do not gain an exception by copying accounting field names.
Failures are tool errors with a message; the server keeps running.

## Safety model

- **Read-only by default.** Without `--allow-control` the control tools are not
  registered, so a host cannot call them.
- **Control is opt-in per server.** Register a separate, control-enabled server
  entry only for hosts and sessions you trust to change runs.
- **Uncertainty rules are preserved.** `retry_cell` calls the same function as
  `irrigator retry`. `allow_uncertain` and `confirm_stopped` have no defaults and
  keep the CLI meaning. Supply both Booleans: `allow_uncertain:true` authorizes
  duplicate external effects for uncertain work; `confirm_stopped:true` is
  required when cleanup remains unconfirmed. Supplying a Boolean is distinct
  from attesting `true`. Work classified uncertain is never automatically
  replayed. Reconciliation can return an interrupted, declared replay-safe cell
  to pending when its cleanup and budget conditions permit; `retry_failed`
  only adds replay-safe failed cells to that work.
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
