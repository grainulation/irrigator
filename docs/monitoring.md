# Read-only observations and judgments

Use an ordinary task to capture an external system at a specific time, then pass
selected evidence to a dependent judgment task. A verified sensor task means its
read and output contract succeeded. It does **not** mean the issue, deployment,
or other monitored target is finished.

The [runnable example](../examples/monitoring/plan.py) uses the existing
[`swarm.worker/v1` command-provider protocol](provider-protocol.md). It makes one
bounded GitHub issue read and compares the returned states with a proposed
expected state. It needs Python 3.9+, the built Irrigator CLI, and an installed,
authenticated GitHub CLI (`gh`). It makes no model calls.

## Run one observation

Authenticate `gh` through your host's normal login flow and check `gh auth status`.
Do not place credentials in plans or source files. The generated sensor provider
explicitly allows environment **names** for `PATH`, `HOME`, `GH_CONFIG_DIR`,
`GH_HOST`, `GH_TOKEN`, `GITHUB_TOKEN`, `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`.
Values come from the launching host. Omit unneeded grants before admission. The
judge adds no environment grants and performs no GitHub reads. Command providers
also receive the runtime baseline environment (`PATH`, `HOME`, `TMPDIR`, `TEMP`,
`SystemRoot` when present); these adapters are not an OS sandbox.

From the repository root, substitute a repository and issue you can read:

```sh
mkdir -p runs
python3 examples/monitoring/plan.py --repo OWNER/REPOSITORY --issue 123 \
  --expected-state CLOSED > runs/issue-observation.json
./irrigator plan runs/issue-observation.json
./irrigator apply runs/issue-observation.json --run runs/issue-observation-1 \
  --backend sensor --workers 2 --timeout 20s --call-budget 0.01
./irrigator inspect runs/issue-observation-1 sensor
./irrigator inspect runs/issue-observation-1 judge
```

The positive per-call limit satisfies CLI admission; this example reports zero
model cost and does not invoke a paid provider. GitHub access remains subject to
your account's permissions and rate limits. The generator embeds absolute Python
and adapter paths because providers execute in task cells, not the repository
root. Generate a new plan if those paths change; do not edit an admitted plan.
Use a fresh run directory for each observation.

Replace `--issue 123` with `--search 'label:ready'` for a repository-local search.
The adapter reads at most 20 issues with a timeout of at most 15 seconds, also
bounded by the request timeout. Search results are a bounded sample, not proof
about every matching issue. Searches cannot override the selected repository.
There is no automatic polling, watch-node lifecycle, or new scheduler state.
Repeat observation explicitly when fresh evidence is needed.

## Keep facts, proposals and acceptance separate

The plan pins two disjoint context slices. `sensor.context` receives only
`current_facts`, the caller's repository and issue/search selection. This names
what to observe; the actual current state comes from the returned snapshot.
`judge.context` receives only `proposed_design`, the desired `expected_state`.
Neither task automatically receives all plan context.

The sensor returns a **string-valued** `snapshot` containing JSON with source,
exact CLI arguments, target, request/operation identity, `observed_at` in UTC and
returned data. `judge.depends_on` gates execution on sensor acceptance;
`judge.input_fields` selects only `sensor.snapshot`, excluding its separate
observation note. Preserve these distinctions when adapting the example.
[Workflow composition](workflow-composition.md) explains stronger typed artifact
contracts for larger graphs.

The deterministic judge emits `verdict` and string-valued JSON `evidence`:

- `pass`: all observed issue states match the proposed state.
- `fail`: none match.
- `mixed`: some match and some do not.
- `inconclusive`: a successful search returned no issues to assess.

These are domain judgments inside accepted artifacts. A verified judge may
legitimately return `fail`, `mixed` or `inconclusive`; execution success is separate
from the verdict. Evidence retains source, timestamp, issue URLs and scope, so a
reader can distinguish an observation from a proposal or a later state.

A failed, timed-out or malformed GitHub read returns a protocol error without
successful output. It cannot become an accepted empty snapshot and release the
judge. Valid request identities receive matching JSON error responses; malformed
JSON or unusable identities fail closed on stderr. Stdout contains protocol JSON
only. The adapter runs fixed read-only `gh issue view`/`gh issue list` commands
without a shell. Adaptation to write operations requires separate authority,
idempotency and recovery design.

## Verify without credentials

```sh
make build
python3 -I verification/monitoring-sensor-test.py
```

The test replaces `gh` with a local fixture, exercises malformed requests,
read failures, timeouts and verdicts, and executes the actual Irrigator
sensor-to-judge graph. It checks selected context and dependency fields in the
runtime's saved requests. No GitHub or model calls are made.

## Watching a run from tools and agent workspaces

### Event stream: `irrigator logs --json --follow`

`irrigator logs RUN_DIR --json --follow` is the stable machine-readable event
stream. It prints one JSON object per line:

```json
{"seq":12,"event":{"time":"2026-01-02T03:04:05Z","kind":"...","task":"build","detail":{}}}
```

- `seq` is the event's 1-based position in the run's append-only event list. It
  never changes and never repeats, so it is the cursor.
- `event.time` is RFC 3339 UTC, `event.kind` names the transition, and
  `event.task` is the cell id (omitted for run-level events). `event.detail` is
  optional and its shape depends on `kind`; treat unknown kinds and detail
  fields as extensible.
- Events arrive in `seq` order with no gaps. `--follow` polls the run and keeps
  streaming until interrupted; without it the command prints what exists and
  exits. `--task ID` filters to one cell without renumbering `seq`.
- To resume after a disconnect, pass the last `seq` you processed as
  `--since N`; the stream continues with `seq` N+1. `--since` with `--follow` is
  the supported way to tail a run without replaying history.

The stream reports what the run recorded. An absent event is not evidence that
nothing happened, and a cell whose last event is a start may be running, stopped
or uncertain: check `irrigator status` or `inspect` before acting.

### Terminal status: OSC 7501

The live view (`apply`, `resume` and `status --watch` on a terminal) also sends
Program Status Protocol records that Ghostty, herdr-core and similar workspaces
display. Each record is `ESC ] 7501 ; key=value:key=value ESC \`. Fields are
separated by `:`; `title` and `msg` are base64.

| Record       | Fields                                                                                                          |
| ------------ | --------------------------------------------------------------------------------------------------------------- |
| Run (root)   | `state` (`working`, `done`, `blocked`, `idle`), `app=irrigator`, `progress` (0-100, only while working), `title`, `msg` |
| Cell (child) | the same fields, plus `id=<cell>` (`:`, `=` and non-printable or non-ASCII characters in the id become `_`)      |

Cell records map the cell status: `running` and `produced` are `working`,
`verified` is `done`, `failed` is `error`, `uncertain` is `blocked` (a person
must decide), and `cancelled` is `idle`. Pending and waiting cells send nothing.
A record is sent only when it changes. A cell that stops being reported (for
example, on a retry) and any cell still `working` when the view closes receives
`state=idle:app=irrigator:id=<cell>` to clear it. Messages carry only the
status, never a cell's prompts, outputs, errors or costs.

`IRRIGATOR_STATUS=off` disables all of these records, root and cell.

### Title, progress bar and notifications

Beside OSC 7501, the live view sends three widely supported signals. Each is
sent only when it changes, and terminals that do not know a sequence ignore it.

| Signal | Sequence | What it shows |
| ------ | -------- | ------------- |
| Title | OSC 2 | `irrigator <dir> · 3/5 verified · 1 running`. The previous title is saved first (XTWINOPS 22) and restored when the view closes (23), where the terminal supports the title stack. |
| Progress bar | OSC 9;4 | The verified percentage while working, shown paused while blocked; removed when the run is done, idle or the view closes. Ghostty, Windows Terminal and others show it in the tab or taskbar. |
| Notification | OSC 9 or OSC 777 | A desktop notification when the run completes, when it becomes blocked, and when a cell becomes uncertain. Never on the first render, so watching an already finished run stays quiet. |

One notification form is sent, so terminals that read both do not show it
twice: OSC 777 under VTE terminals, Konsole and foot, otherwise OSC 9 (Ghostty,
iTerm2, kitty, WezTerm). `IRRIGATOR_NOTIFY=osc9` or `osc777` forces a form and
`IRRIGATOR_NOTIFY=off` turns notifications off. `IRRIGATOR_STATUS=off` turns
off all of these signals as well. Like the 7501 records, they carry only
counts and states, never prompts, outputs, errors or costs beyond the run
total already shown in the view.

Measured on herdr-core 0.1.19: it records the OSC 2 title (`hc get`, `hc ls`,
`title` events) and OSC 7501, and ignores OSC 7, OSC 9;4 and notifications.
Its reported working directory comes from the process, not OSC 7.

### Notifications for unattended runs

The OSC notifications above exist only while a terminal view is attached. A run
started detached (by `irrigator mcp`, or `resume` in the background) can instead
tell you through ntfy or a webhook:

```sh
export IRRIGATOR_NTFY_TOPIC=<long random topic>   # token only via IRRIGATOR_NTFY_TOKEN
irrigator apply plan.json --run RUN --backend mock --notify ntfy
irrigator resume RUN --notify webhook --notify-url https://hooks.example.com/x
```

The controller sends one message when the run becomes **blocked** or
**complete**, and when a cell becomes **uncertain** or **failed**, whether or not
a terminal is attached. It uses the same notifiers as `approvals notify`, so
`IRRIGATOR_NTFY_SERVER`, `IRRIGATOR_NTFY_TOPIC`, `IRRIGATOR_NTFY_TOKEN` and
`IRRIGATOR_APPROVALS_WEBHOOK` apply.

| Source | Precedence (first match wins) |
| ------ | ----------------------------- |
| `--notify ntfy\|webhook\|off` | the flag |
| `RUN/notify.json` | the choice recorded by an earlier start |
| `IRRIGATOR_NOTIFY_VIA` | the environment |
| none | off |

`RUN/notify.json` holds the channel and any `--notify-topic` / `--notify-url`
you passed, mode 0600. It never holds a token, and a topic or webhook URL taken
from the environment is not copied into it. `RUN/notify-sent.json` lists the
transitions already announced, so a resumed controller does not send them again
(resuming a finished run sends nothing new; a new attempt that ends blocked or
complete again does). A bad
`--notify` value fails the command; a bad recorded or environment choice only
disables notifications with a warning.

A message names the run, the verified/failed/uncertain counts, the cell id and
status, the run cost total and `irrigator status <run>`. It never contains
prompts, outputs, error text or per-cell costs. Sends run off the scheduling
path with a 5 second timeout; the controller waits at most 3 seconds for them
when it exits. A failure is written to `RUN/notify.log` and stderr and never
changes scheduling or task state. A transition is recorded before its send, so a
failed send is not retried: the status command remains the source of truth.

### Links, clipboard and semantic marks

Three more terminal signals follow the same rules: written only to a terminal
(or, for OSC 133, only when worker progress is on), ignored by terminals that
do not know them, and carrying no prompts, outputs, errors or credentials.

| Signal | Sequence | Where | Terminals |
| ------ | -------- | ----- | --------- |
| Hyperlinks | OSC 8 | See below | iTerm2, Ghostty, kitty, WezTerm, VTE (GNOME Terminal), Konsole, foot, Windows Terminal, Alacritty 0.11+. Apple Terminal does not. |
| Clipboard | OSC 52 | Review TUI keys `y` and `Y` | iTerm2 (enable "Applications in terminal may access clipboard"), kitty, WezTerm, Ghostty, foot, Alacritty, Windows Terminal. Under tmux set `set-clipboard on`; over ssh it reaches your local clipboard. Apple Terminal does not. |
| Semantic marks | OSC 133 | Worker terminals with `IRRIGATOR_WORKER_PROGRESS=1` | iTerm2, kitty, WezTerm, Ghostty, VTE 0.76+, Windows Terminal. |

**Links (OSC 8).** On a terminal, and unless `IRRIGATOR_LINKS=off`:

- the live view (`irrigator status --watch`) prints `run` and `cells` lines
  when it closes, each a `file://` link (with this machine's hostname, as OSC 8
  expects) to the run directory and its per-cell logs;
- `irrigator review list` links each branch to the worktree where it is checked
  out, and each plan or decision to its snapshot directory;
- `irrigator approvals link` links the printed `/approve/TOKEN` path to
  `$IRRIGATOR_APPROVALS_URL` plus that path, for example
  `IRRIGATOR_APPROVALS_URL=http://127.0.0.1:8787`. Without that variable it
  prints the path as before.

Only `http`, `https` and `file` URLs link, and control characters are removed
from both the URL and the text. Output to a pipe, a file or `--json` is
byte-for-byte what it was before. Links are not color, so `NO_COLOR` does not
turn them off; `IRRIGATOR_LINKS=off` (or `0`, `false`) does.

**Clipboard (OSC 52).** In `irrigator review tui`, `y` copies the selected
branch name and `Y` copies `git diff <base>...<branch>` (quoted when the name
needs it). The status line shows `copied`, and the keys are listed in the help
footer. Copies longer than 1024 bytes are refused. Nothing is sent to the
terminal unless you press the key.

**Semantic marks (OSC 133).** A worker with progress on wraps each attempt like
a shell command: `A` then `B` before the `admitted` line, `C` before the
provider starts, and `D;0` after the outcome line when the attempt settled or
`D;1` when it failed or is uncertain. Terminals that read these let you jump
between attempts (for example iTerm2 and kitty's previous/next prompt, Ghostty's
`jump_to_prompt`) and select one attempt's output. With progress off nothing is
written.

### Worker progress lines

With `IRRIGATOR_WORKER_PROGRESS=1` a worker prints short lines to stdout as an
attempt moves, for example:

```text
irrigator: build attempt 1 admitted
irrigator: build attempt 1 provider started
irrigator: build attempt 1 settled, cost $0.0123
```

The outcome is `settled`, `failed` or `uncertain`, and the cost reads
`cost unknown` when the provider did not report one. The herdr launcher
(`IRRIGATOR_EXECUTOR=herdr`) sets this for its workers so `hc attach` and
`hc read` show progress, and each line also sets the terminal title (OSC 2)
to the same text, so `hc ls` shows every worker's current state; the local executor never sets it, so the dashboard is
unaffected. The lines contain no prompts, outputs, errors or credentials.

## How run cost is measured

Reported costs, run totals, provider ledger reservations and budget checks are
exact integers in micro-dollars (1 USD = 1,000,000 micros). A cost a provider
reports with more than six decimal places is rounded half to even to whole micros
once, when it is read from the raw JSON number, and negative, non-finite or
out-of-range values are rejected. Per-task state stores `reported_cost_micros`
beside `reported_cost_usd`; older state without it derives the micros from the
float with the same rounding. JSON fields named `*_usd` remain numbers for
compatibility and are the micro total divided by 1,000,000.

Each attempt records a `cost_basis`: `reported` when `cost_usd` was sent
(including 0), or `unknown` when omitted. Token usage is recorded as its own
measurement independently in durable state (`usage` with `input_tokens`,
`output_tokens`, `thinking_tokens`, `cache_read_tokens`, `cache_write_tokens`,
`total_tokens`, and optional `model`). Cells that reported tokens but no USD
cost are never shown as $0: `irrigator status` displays "cost unknown" (and
tokens when known) for unknown cells. Run totals and `irrigator cost RUN [--json]`
report known reported spend, the count of unknown-cost operations, and token
totals separately.

## Export run telemetry to OpenTelemetry

`irrigator otel export RUN [--follow] [--since N] [--endpoint URL]` sends a run's
sequenced events (the same source as `irrigator logs --json`) to an OTLP/HTTP
collector as traces. It uses the official OpenTelemetry Go SDK.

- One trace per run; one span per cell attempt, with start and end taken from
  the events and status from the outcome.
- Span attributes: `irrigator.run_id`, `irrigator.cell`, `irrigator.attempt`,
  `irrigator.outcome`, and `irrigator.cost_usd` when known. Other events are
  span events.
- Resource: `service.name=irrigator`, `service.version`, `irrigator.plan`.
- Trace and span IDs are derived from the run id, cell and attempt, so
  re-exporting the same events does not create duplicates.
- Prompts, outputs, file contents, credentials and user identity are never
  exported. `OTEL_RESOURCE_ATTRIBUTES` and `OTEL_SERVICE_NAME` are not merged
  into the exported resource.
- `--follow` tails new events; `--since N` resumes after event sequence N.
  SIGINT flushes pending spans and exits cleanly.

### Setup with a local collector

Run a collector with an OTLP/HTTP receiver, for example
`otel-collector.yaml`:

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 127.0.0.1:4318
exporters:
  debug:
    verbosity: detailed
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [debug]
```

```sh
otelcol --config otel-collector.yaml
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
irrigator otel export RUN            # one-shot
irrigator otel export RUN --follow   # tail until Ctrl-C
irrigator otel export RUN --since 120
```

Endpoint, headers, timeout and certificates come from the standard
`OTEL_EXPORTER_OTLP_*` variables; only `http/protobuf` is supported. Put
credentials in `OTEL_EXPORTER_OTLP_HEADERS`, not in `--endpoint` (which rejects
embedded credentials and is a base URL; `/v1/traces` is appended). Header values
are never printed or logged.

### Correlate with Claude Code telemetry

To join Claude Code's own telemetry to an Irrigator run, set resource
attributes in the provider's environment:

```sh
OTEL_RESOURCE_ATTRIBUTES=irrigator.run=RUN_ID,irrigator.cell=CELL_NAME
```

Claude Code then tags its telemetry with `irrigator.run` and `irrigator.cell`,
which match the `irrigator.run_id` and `irrigator.cell` attributes on exported
spans.

### Remove identity before export

Claude Code telemetry includes `user.email`, `user.account_uuid` and
`organization.id`. Delete them in the collector before any exporter:

```yaml
processors:
  attributes/strip_identity:
    actions:
      - key: user.email
        action: delete
      - key: user.account_uuid
        action: delete
      - key: organization.id
        action: delete
  resource/strip_identity:
    attributes:
      - key: user.email
        action: delete
      - key: user.account_uuid
        action: delete
      - key: organization.id
        action: delete
service:
  pipelines:
    traces:
      processors: [attributes/strip_identity, resource/strip_identity]
    metrics:
      processors: [attributes/strip_identity, resource/strip_identity]
    logs:
      processors: [attributes/strip_identity, resource/strip_identity]
```

Attributes can sit on the data point/record or on the resource, so the example
deletes both. Irrigator's own export does not carry these fields.
