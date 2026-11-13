# Plugin architecture

The plugin is a Markdown workflow layer over the real `irrigator` CLI. Its
manifests describe loading and presentation; the executable and durable run files
own execution. See [Claude Code](claude-plugin.md), [Codex](codex-plugin.md),
[host integration](host-integration.md), and [packaging](local-release.md).

## Principles

1. **The CLI is the source of truth.** Skills only instruct an agent to run documented `irrigator` commands and read durable run files. Skill names and commands must match the executable's `--help` output. Notation: [runtime notation](runtime-notation.md).
2. **Actual capabilities only.** No permissions broker, no pricing and no public release claim. The Claude plugin registers only the read-only `irrigator mcp` server ([MCP server](mcp.md)); Codex users add it by hand.
3. **Local first.** Runs and plans are ordinary local files. The runtime (the `irrigator` CLI and its durable run files) is primary and authoritative; the observe skill and the read-only `irrigator mcp` server are secondary observers that never replace `status`, `inspect` or `logs`. Nothing sends remote messages, pushes, publishes or creates domains automatically.
4. **Host permissions are respected.** A skill that needs a shell command asks the host to run it under the host's own approval rules. It never tries to bypass a denied command, and never edits `state.json` or other run files by hand.
5. **Markdown workflow, no host-only tools.** Codex follows the Markdown workflow manually. Skills must not assume a Claude-only `Skill` tool; to chain skills, read the sibling `skills/<name>/SKILL.md` file directly.

## Layout

```text
irrigator/
  plugin.json                  portable root manifest (Agent Plugins schema)
  .claude-plugin/
    plugin.json                Claude Code manifest
    marketplace.json           local marketplace entry (not a published listing)
  .codex-plugin/
    plugin.json                Codex compatibility manifest, skills: "./skills/"
  skills/
    irrigator/SKILL.md         entry point: what Irrigator is, where to go next
    router/SKILL.md            intent router to the skills below
    plan/SKILL.md              build and validate a plan from a shape or task list
    run/SKILL.md               preflight, authorize, apply, verify, export
    resume/SKILL.md            recover interrupted or uncertain runs
    observe/SKILL.md           status, logs, graph
    budget/SKILL.md            sizing dry run and spending authorization
    swarm/SKILL.md             route a swarm request to its stage
    debug/SKILL.md             read-only run diagnosis
  docs/                        reference documents (including this page)
  tools/package-plugin.py      local packaging helper
  verification/plugin-contract-test.py
  verification/plugin-validator-regression.py
```

Resources resolve from the plugin root, not from inside `.codex-plugin/`. The portable root `plugin.json` plus root `skills/` and the `.codex-plugin/plugin.json` compatibility layout can coexist. A manifest may declare only the read-only `irrigator mcp` server (no `--allow-control`); the contract test enforces this.

## Skills and the CLI they wrap

| Skill | Purpose | CLI commands it may drive |
| --- | --- | --- |
| irrigator | Orientation and entry point | `version`, `doctor`, `--help` |
| router | Map a plain request to a skill | none (routing only) |
| plan | Turn a shape or task list into an explicit plan | `init`, `plan`, `size --shape` |
| run | Execute under explicit authorization | `plan`, `size`, `prepare`, `apply`, `deliver`, `status`, `inspect`, `export` |
| resume | Recover, retry, resolve uncertainty, revise | `resume`, `retry`, `resolve`, `cancel`, `pause`, `down`, `propose`, `decisions`, `policy`, `gc` |
| observe | Read-only inspection | `status`, `inspect`, `graph`, `logs`, `events`, `artifacts`, `decisions` |
| swarm | Route a request to its workflow | no separate CLI command |
| debug | Diagnose read-only | `debug`, `status`, `inspect`, `logs` |
| budget | Estimate and bound spend | `size`, `apply --sizing`, `resume --sizing` |

`events` and `artifacts` are read-only commands that `internal/cli/dispatch.go` dispatches (each takes one run directory and no flags) but that `--help` does not list.

### Intent routing

| User intent | Skill |
| --- | --- |
| "what is Irrigator", "is it installed" | irrigator |
| "split this into N agents", "25x5x2", "make a plan" | plan |
| "run it", "execute the plan", "launch the swarm" | run |
| "it crashed", "continue", "retry", "uncertain" | resume |
| "where are we", "watch", "show the graph", "logs" | observe |
| "how much will it cost", "how many workers" | budget |

Routing preserves the active task and asks only for information that materially changes the work. Plain code edits and general conversation are not routed.

## Required behaviors, by skill family

The plugin contract tests check the following workflow requirements. Each maps to a runtime fact documented in [runtime notation](runtime-notation.md).

| Behavior | Runtime basis |
| --- | --- |
| Use first-class public run/task metadata (plan and task `display`) rather than ad hoc names | display limits: plan title ≤60, purpose ≤140; task title ≤48, why_spawned ≤100, done_when ≤140 |
| Keep shape separate from concurrency | `--workers` is independent of A×D×S; waiting ancestors take no worker slot |
| Exact source preflight | read the exact plan and source files before launch; `plan PLAN` validates; `doctor` for the CLI |
| Paid budget authorization | `--budget` is per-call, not a graph cap; state the enforced aggregate cap and its mechanism; default to `--backend mock` when no authorization exists |
| No blind uncertain replay | `uncertain` nodes need `resolve` or explicit `retry --allow-uncertain` from the user |
| Preserve immutable plans | change unfinished work only via `propose`; never rewrite the original plan or hash |
| Actual dispatch evidence | cite `status --json`, `inspect`, `logs --json`, recorded costs |
| Independent acceptance | verified results plus a command validator or separate integration gate; a model call is not acceptance |

## Host differences

| Aspect | Claude Code | Codex |
| --- | --- | --- |
| Manifest | `.claude-plugin/plugin.json`, `marketplace.json` | root `plugin.json`, `.codex-plugin/plugin.json` |
| Skill invocation | host skill mechanism may exist, but skills must not rely on it | follow `skills/*/SKILL.md` manually |
| Shell | host asks the user for permission | host asks the user for permission |
| MCP | read-only `irrigator mcp` in `mcpServers` ([MCP server](mcp.md)) | none declared; user adds a `config.toml` entry |

On either host the agent must verify that the `irrigator` executable is on PATH (`irrigator version`) and fall back to building it from a checkout (`make build`, Go 1.26.7+ for the complete runtime) only when the user wants that. Installation of the binary or the plugin into a host is the user's action, not an automatic step.

## Distribution boundary

The source is maintained at
[grainulation/irrigator](https://github.com/grainulation/irrigator) under the
[MIT license](../LICENSE). Packaging does not publish a marketplace listing,
install into an agent host, or establish model invocation. It ships no
permissions broker or runtime binary. Mock runs demonstrate orchestration;
model quality and speed must be assessed separately.

## Failure handling summary

| Situation | Action |
| --- | --- |
| `irrigator` not found | tell the user; do not invent output; offer `make build` from a checkout |
| CLI found but not authenticated | `doctor` reports it; do not call paid backends; offer `--backend mock` |
| Plan fails `plan PLAN` | fix the plan; do not launch |
| Shape maximum exceeds 1024 nodes or D>16 | reject the shape; propose a smaller one |
| Budget or authorization missing | stop before any paid call |
| Node `uncertain` | inspect externally; `resolve` or user-approved `retry --allow-uncertain` |
| Expansion stopped by `max_tasks` or spend | report the smaller realized graph; never call it a full tree |
| An event tail or MCP view disagrees with `status` | durable state wins; the event tail is a bounded window |
