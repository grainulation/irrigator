# Your first run

Start with mock workers, then choose a real provider explicitly. These steps use
the CLI from the repository root after `make build`. Use new filenames and run
directories; Irrigator preserves existing evidence.

## 1. Run without credentials

```sh
./irrigator demo --run ./runs/first-demo
./irrigator status ./runs/first-demo
./irrigator inspect ./runs/first-demo discover
```

The terminal displays task states as the graph grows. `IRRIGATOR_UI=static` keeps
live state updates without animation; `IRRIGATOR_UI=plain` uses ordinary logs.
See [Linux terminal controls](linux.md#terminal-controls).

The live view also reports the run's state (working, blocked, done, idle) and
progress with OSC 7501, the Program Status Protocol. Terminals and workspaces
that support it, such as Ghostty and herdr-core, show it in a tab, sidebar or
`hc get`; others ignore it. It also sets the tab title, shows a progress bar
(OSC 9;4) and sends a desktop notification (OSC 9 or 777) when the run
completes or needs you. `IRRIGATOR_NOTIFY=off` silences notifications;
`IRRIGATOR_STATUS=off` turns all of these off. See
[Monitoring](monitoring.md#title-progress-bar-and-notifications).

Optionally, with [herdr-core](https://herdr.dev/core/) 0.1.19 or newer
installed, `--executor herdr` (or `IRRIGATOR_EXECUTOR=herdr`) runs each worker
in its own `hc` terminal, labelled with its run, cell and attempt, so `hc ls`
and `hc attach` or the Herdr app can watch it. Workers keep `PATH` and
`IRRIGATOR_*` settings but never receive credentials through `hc`, so model
providers must already be signed in where herdr-core runs. Client logs go to
`<run>/herdr/`. `irrigator doctor` shows which executors are available. See
[Executors](executors.md).

## 2. Make your own plan

```sh
mkdir -p runs
./irrigator init --out ./runs/first-plan.json
./irrigator plan ./runs/first-plan.json
./irrigator apply ./runs/first-plan.json --run ./runs/my-first-plan --backend mock
./irrigator export ./runs/my-first-plan --out ./runs/first-results.json
```

A plan names tasks, their inputs, required results, and dependencies. A verified
result satisfies the configured acceptance checks; review the content before
using it. The [plan schema](plan.schema.json) supports editor completion;
`irrigator plan` performs the authoritative runtime validation.

The CLI requires an explicit backend for unmanaged plans. Omitting it fails before a run
is created. Managed plans instead select their explicitly configured execution
profiles. There is no implicit paid backend.

### Include local source context

Create `runs/review-template.json` with a named context reference:

```json
{
  "api_version": "swarm/v1",
  "name": "Source review",
  "tasks": [{"id": "review", "prompt": "Review the source for recovery risks.", "requires": ["answer"], "context": ["source"]}]
}
```

Materialize an explicitly selected file into a new plan before validating or
running it:

```sh
./irrigator plan ./runs/review-template.json --context-file source=./README.md --out ./runs/review-plan.json
./irrigator plan ./runs/review-plan.json
```

Repeat `--context-file KEY=PATH` for additional files. Each file must be regular
UTF-8 text of at most 1 MiB; the resulting plan is limited to 8 MiB. Existing
context keys and output files are never overwritten. Only named files are read:
there is no glob expansion by Irrigator, environment substitution, or automatic
workspace scan. The original plan stays unchanged. Imported contents are stored
in the new private plan, not reread at dispatch. Review those contents before
sending the plan to a provider; this command itself makes no model calls.

## 3. Optional: one real model call

This step spends money. First install and log into Claude Code through its normal
login flow. The built-in backend uses that login and a restricted environment;
it does not inherit arbitrary API keys from your shell.

Create `runs/one-call.json` (create `runs/` first if needed):

```json
{
  "api_version": "swarm/v1",
  "name": "One bounded model call",
  "tasks": [
    {"id": "summary", "prompt": "Explain durable task execution in one sentence.", "requires": ["answer"]}
  ]
}
```

Preview it, then explicitly select the paid provider:

```sh
./irrigator plan ./runs/one-call.json
./irrigator apply ./runs/one-call.json --run ./runs/one-call --backend haiku --workers 1 --call-budget 0.10
./irrigator inspect ./runs/one-call summary
```

`--call-budget` is the requested limit for each provider invocation, not a total
workflow cap or a guarantee about the provider invoice. `--budget` remains an
alias. For managed run-level admission reservations and mixed providers, move on
to [execution profiles](execution-profiles.md). Cost reports retain unknown
charges separately from reported spend.

`doctor.ready` and `local_ready` describe the credential-free local engine.
`model_ready` describes the checked Claude login; it does not certify every
named provider. Check the provider-specific diagnostics before model-backed work.

### Scaffold a managed provider plan

For explicit model selection and best-effort cost tracking, create a managed
plan using the installed Claude executable:

```sh
mkdir -p runs
./irrigator init --managed --profile claude:sonnet --out ./runs/first-managed.json
./irrigator plan ./runs/first-managed.json
```

This offline step resolves the installed executable and records its checksum. It
does not log in or make a provider call. Inspect the generated task, timeout, and
tracking threshold and provider per-call limit before explicitly starting paid work:

```sh
./irrigator apply ./runs/first-managed.json --run ./runs/first-managed
```

The plan contains local executable paths and is configuration for your machine;
keep it out of public source control. See [execution profiles](execution-profiles.md)
for supported providers and model settings.

## 4. Pause and recover

Start another mock demo in one terminal:

```sh
./irrigator demo --run ./runs/recovery-demo
```

While it is running, use a second terminal:

```sh
./irrigator pause ./runs/recovery-demo
./irrigator status ./runs/recovery-demo
./irrigator resume ./runs/recovery-demo
```

If the small demo finished before the pause, it has no unfinished work to resume.
Ctrl-C during execution also requests a safe pause. `status --watch` is an
observer: stopping it does not stop the run. If the controller disappears, the
watch exits with recovery guidance instead of repeatedly displaying stale state.

A sibling `<run>.init.lock` file is a persistent coordination lock, not an active
process marker. Keep it in place: unlinking lock files while another process may
hold or open them can allow two initializers to lock different files for the same
run.

After an unexpected crash, inspect `status` and `debug` before resuming. An
interrupted paid call can be `uncertain`: do not retry until you have investigated
its external outcome and any surviving process. See [recovery](recovery.md) and
[security](../SECURITY.md) for explicit reconciliation and retry boundaries.

## Next steps

- Use the [Claude Code](claude-plugin.md) or [Codex](codex-plugin.md) skills.
- Add [main-agent turns and terminal helpers](turn-shapes.md).
- Configure [providers and run budgets](execution-profiles.md).
- Compose [reusable workflows](workflow-composition.md).
