# Bounded graph scale checks

Plans may explicitly set `max_tasks` up to **10,000**. Omitting it still selects
**128**. Graph capacity does not change the worker limit, provider allowance,
maximum depth, input/output byte bounds or spending policy. Older binaries can
reject plans exceeding their supported ceiling; no plan fields or protocol names
change merely because this ceiling is larger. Preserve the original admitted
plan and create a new run to change its capacity.

The scale harness exercises the real execution engine. Mock and local-model
modes are separate and are identified in every report. It does not measure
model quality or establish that local compute has a known dollar cost.

## Small calibration

Build the intended source revision, then choose a new directory:

```sh
make build
./irrigator benchmark --tasks 12 --mode mock --workers 6 --dir ./new-mock-scale
```

Without `--tasks`, `benchmark --dir NEW --runs 2` retains the original fixed
five-task serial/parallel comparison. Scale mode creates one workload per
invocation; it does not accept `--runs`.

For an explicitly authorized local model, use an already running trusted Ollama
server and an already installed GGUF model. Obtain its exact installed tag and
digest from that server's inventory. The harness does not download a model,
start a server, select a cloud fallback or call another provider.

```sh
./irrigator benchmark --tasks 4 --mode ollama --workers 1 \
  --endpoint http://127.0.0.1:11434 --model INSTALLED_TAG \
  --model-digest LOWERCASE_SHA256 --max-output-tokens 32 --timeout 30s \
  --dir ./new-local-calibration
```

The adapter enforces numeric loopback addressing, digest checks before and after
generation, no tools, bounded input/output bytes, token ceiling and per-task
timeout. See [local Ollama guarantees and limitations](managed-ollama.md).

## Qualify 900, 2,000 and 10,000 tasks

The orchestration driver runs each selected size once, stops after a failed case
and never automatically replays failed or uncertain work:

```sh
python3 tools/scale-check.py --binary ./irrigator --dir /tmp/new-scale-check \
  --mode ollama --endpoint http://127.0.0.1:11434 \
  --model INSTALLED_TAG --model-digest LOWERCASE_SHA256 \
  --counts 900,2000,10000 --workers 6 --max-output-tokens 32 \
  --timeout-seconds 30 --wall-timeout 7200
```

This requests **12,900 actual local generations**, with a maximum of 412,800
generated output tokens, at most six active workers, 30 seconds per task and two
hours per case. The token ceiling does not bound prompt evaluation; input bytes
are capped separately. Use `--counts 10000` for only the 10,000-task workload or
`--mode mock` without model flags for the deterministic engine counterpart.

The driver samples the controller process tree every half-second. An optional
`--server-pid PID` samples an owned local Ollama server separately. RSS is a
sampled sum, not an exact peak. Waited-child CPU and OS output-block counters
include the benchmark and sampling subprocesses; they are not physical disk-byte
measurements or Ollama server CPU measurements. Run with the local permissions
needed for process inspection; missing metrics remain explicitly visible.

The driver bounds controller logs to 64 MiB by default. On an interrupt, wall
timeout or log limit it asks its owned controller to pause and drain. It never
signals historical PIDs or restarts an interrupted run automatically. If the
controller cannot be drained, cleanup uncertainty is recorded for inspection.

## What passes

`scale-report.json` records task/model identity, bounded generation settings,
validation, initialization, execution, completed-read, resume, run-ID lookup,
backup and restored-read timings. Acceptance requires:

- Every task is verified with the expected structured output, exactly one
  attempt and generation, and no automatic replay.
- Local mode has one matching model/token-usage receipt per task; no mock result
  can satisfy that gate. The workload measures delivery and execution, not model
  intelligence.
- Reopening/resuming the completed run preserves attempts, results and usage.
- Short run-ID lookup succeeds; backup verifies; the quarantined restore retains
  the admitted plan, run identity, results and usage.

The engine report includes application storage counters from its controller
process only. Worker-process counters and physical filesystem traffic are not
included. `run_footprint_bytes` is the final logical file footprint, not cumulative
writes. The driver adds process measurements and logs in `scale-check.json`.

Non-interactive controller output shows at most 20 cells per update by default.
`IRRIGATOR_DISPLAY_CELLS` can select 1–200; summaries retain complete counts and
show how many cells were omitted. Inspect an individual task with `irrigator
inspect RUN TASK_ID`; bounded display does not discard saved evidence.

Record the source revision, built binary, operating system and local runtime
alongside a qualification report. Passing admission/unit tests is not evidence
that a 10,000-generation live run completed. A result from one hardware profile
does not establish a universal CPU, memory or latency budget.
