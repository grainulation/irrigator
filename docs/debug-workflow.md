# Irrigator debug workflow

`irrigator debug` is read-only diagnosis of one durable run, plus a skill (`skills/debug/SKILL.md`) that routes between `status`, `graph`, `inspect` and `debug`. It is Irrigator-specific. It is not a universal `/swarm:debug` command.

## Command

```sh
irrigator debug RUN_DIR [--json] [--node ID]
```

- Reads `state.json` once through the existing checkpoint reader. It starts no provider, spends nothing, takes no lock, writes nothing and creates no file. The lease probe is non-blocking and only opens existing lease files.
- Exit 0: diagnosed. Exit 1: run unreadable or unknown node. Exit 2: usage error. A run with failed nodes still exits 0, because debug observes.
- Text output is for people. `--json` prints schema `irrigator.debug/v1`.

## What is projected

| Field | Source |
| --- | --- |
| `control`, `backend`, `graph_revision`, `reported_cost_usd`, `status_counts` | checkpoint |
| `workers_active`, `workers_known` | read-only lease probe; unknown stays unknown |
| `attention[]` (cap 25, sorted by ID) plus `attention_total` and `attention_omitted` | nodes that are failed, uncertain or cancelled, or waiting owners with blockers |
| `recovery` | the existing lifecycle recovery projection: classification, advice, blocker and active IDs with totals and omissions |
| `classification` | the shared recovery constants, from status only |
| `attempt`, `generation`, `error_code`, `error_recorded` | added here because the shared projection lacks them |
| `recent_events` (newest 10) | the runtime's allowlisted event projection; no event detail |
| `selected` | the node named by `--node`, of any status |

`error_code` is a fixed vocabulary: `outcome_uncertain_<reason>` for the runtime's fixed uncertain message, `validator_uncertain`, `outcome_uncertain_unclassified`, `failed_error_not_projected`, `failed_no_error_recorded`, `cancelled`. Error text, prompts, results and provider configuration are never printed. `next_steps` are descriptive and execute nothing.

## Replay fixtures (experimental)

```sh
irrigator debug fixture export RUN_DIR --out FILE [--scenario NAME]
```

Experimental: the verb, the `irrigator.replay-fixture/v1` format and `make replay-gate` may change. The export is read-only on the run. It refuses to overwrite `--out` (including through a symlink), refuses an `--out` inside the run directory, refuses a `state.json` that is not a regular file or exceeds 256 MiB, writes the file `0600`, and writes nothing if its own redaction check rejects the result. Event kinds outside the runtime's known vocabulary are exported as the generic `event`. Exit 0: written. Exit 1: run unreadable or refused, `--out` exists or cannot be created, backend not exportable, or redaction rejected the export. Exit 2: usage error. Only `debug fixture export …` selects this verb, so `irrigator debug fixture` still diagnoses a run directory named `fixture`; `irrigator debug fixture export --help` prints the verb's usage. Fixture files with a repeated JSON object key (compared case-insensitively, as Go's decoder matches struct fields) are rejected.

What a fixture holds: anonymous task IDs (`t1..tn`, in plan order), dependency, parent, spawn and turn-role structure, status, attempt, generation, cancel flag, the fixed `error_code` vocabulary above, a known provider diagnostic code, a `failure_phase` (`validation` or `execution`, from the shape of the failed event, never its text), whether a cost was reported or is unknown, per-task event kinds in order, and the recovery decision (attention nodes, waiting-parent blockers, the fixed advice lines). It never holds prompts, results, outputs, error text, exact cost, env, paths, timestamps, pids, operation IDs or real task IDs. Loading rejects unknown versions and unknown fields, and every string must be an ID, a member of a closed set, a fixed code or a fixed advice line.

`make replay-gate` rebuilds each committed fixture (`internal/debug/testdata/replay/dev` and `heldout`) from its recorded data through the real worker path: the mock executor with real validation, a stub provider process for timeouts, malformed responses, provider errors, in-flight cancels and a SIGKILLed worker, spawn graphs with waiting parents, and turn helpers. It asserts the same terminal states, per-task event order, cost flags and recovery decision. Both sets must pass. Limits: an uncertain outcome on the mock backend, or an uncertain reason with no stub that reproduces it, replays through the cell transition reducer instead; plain failures other than provider diagnostics carry no enumerated class in the state (only the phase); managed runs are covered for the mock adapter only, not reservations or held exposure.

## Skill routing and authorization

Diagnosis needs no spending authorization. Retry, resolve, resume and cancel stay in the resume skill. They proceed only inside an authorization the user already gave for that run, backend and cap. A new run, backend or cap, or an unresolved uncertain node, needs an explicit yes. Uncertain work is never replayed by default.

## Host names

Claude Code namespace: `irrigator`, so `/irrigator:debug` when the host discovers the skill. Codex: `$debug` when discovered. Neither discovery path, nor installation, has been tested. There is no global install step, and the skill never installs itself.

## Integration notes (for the integrator, not applied here)

- `internal/cli/command_help.go` registers `debug` in the command table (`debug.DebugCLI`, with its `fixture export` probe).
- `verification/plugin-contract-test.py`: add `"debug": {"json", "node"}` to `COMMAND_FLAGS`, and `debug` to `SKILLS` and `FROZEN_FILES` for `skills/debug/SKILL.md`. Until then, documented `irrigator debug` lines fail the surface check.
- `tools/package-plugin.py`: `skills` and `docs` are already included. Add `'debug'` to `REQUIRED_SKILLS` only once the packaging owner accepts it.
- A shared observer debug helper, if one is later defined, replaces `debugProject` through a single call. None is consumed today.

## Not done here

Nothing was compiled or run when this was written. Go tests are in `internal/debug/debug_cli_test.go`. Native plugin discovery, a real binary run and independent QA are open.
