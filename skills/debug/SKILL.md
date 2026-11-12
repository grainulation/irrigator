---
name: debug
description: Diagnose an Irrigator run read-only. Routes to status, graph, inspect and the debug command, and hands any change to the resume skill. Irrigator-specific; not a universal debug command.
---

# debug: Diagnose an Irrigator run

Use this skill to answer "why is this Irrigator run stuck, failed or uncertain?" without changing it. It is an Irrigator debug skill, not a general-purpose debugger. For code bugs outside an Irrigator run, do not use it.

This file is plain Markdown. A host, a person or an agent can follow it by hand. Run commands with the host shell, respect permission prompts, and report a declined command as not run.

## Names, honestly

The plugin namespace is `irrigator`. On Claude Code the skill is `/irrigator:debug` once the host has discovered it. On Codex it is `$debug` once discovered. `/swarm:debug` is not a command this plugin provides, and no bare `/debug` is claimed. Discovery and installation on a host have not been tested here, and this skill never installs itself globally. If the host does not list it, open this file by path.

## Arguments

$ARGUMENTS

Expected: a run directory (absolute path preferred, a directory with `state.json`). If the text above is still the literal placeholder, use the user's message. If no run is named, ask. Do not guess a run from timestamps. A session registry is read only from a path the user gave.

## Route by question

| The user wants | Use | Changes the run? |
| --- | --- | --- |
| One bounded diagnosis | `irrigator debug DIR [--json] [--node ID]` | No |
| Counts, control state, cost | `irrigator status DIR --json` | No |
| One node, operation, attempts | `irrigator inspect DIR NODE` | No |
| Sequenced history | `irrigator logs DIR --json --since 0 [--task NODE]` | No |
| A live view | `irrigator status DIR --watch` in the terminal | No |
| Plan or graph shape | `irrigator graph DIR` | No |
| Any fix, retry, resolve, pause or cancel | the resume skill (`skills/resume/SKILL.md`) | Yes |

For executor problems (herdr or tmux missing, too old, or a worker window gone), run `irrigator doctor`, which lists each executor as OK, too old, or not installed, and read the executor in `irrigator status DIR`. A missing executor is not an error unless the run was recorded with it. With the optional `irrigator mcp` server connected, `run_status` and `run_logs` give the same read-only evidence.

Start with `irrigator version`, then `irrigator debug DIR --json`. If `debug` is not in the installed binary, say so and fall back to `status DIR --json` plus `inspect`. Report only what a command printed.

## Reading a diagnosis

- `classification` and `advice` come from the runtime's own recovery projection, from status and explicit evidence only. They are descriptive, not an instruction to act.
- `error_code` is a fixed code. Error text, prompts and results are not shown. `failed_error_not_projected` means an error was recorded and withheld, not that it is unknown.
- `uncertain` means a provider call may have caused an external effect or charge. `failed` alone does not prove an effect either way.
- `active_work_remaining` counts pending, running and produced work. A node that is only waiting for children is not itself failed.
- `workers_known: false` means the lease probe was incomplete. Do not report zero workers then. `control: run` with no live lease is not proof of a stall.
- Lists are capped. Quote the `omitted` counts instead of implying the list is complete.

## Authorization

Diagnosis is read-only and needs no spending authorization. It makes no model call and spends nothing.

Anything past diagnosis (`resume`, `retry`, `resolve`, `cancel`, `propose`) goes through the resume skill and its gates. Act only inside an authorization the user already gave in this session for that run, backend and cap. State which authorization you rely on. A new run, a new backend, a higher cap or an unresolved uncertain node needs an explicit yes. A general request to debug is not such a yes.

## Uncertain nodes

Never replay by default. In order: `inspect`, `logs --task NODE`, check the external system yourself, then either `resolve` with a reconciled result or, only after the user explicitly accepted possible duplicate effects for that operation, `retry NODE --allow-uncertain`. Pass `--confirm-stopped` only after the processes are verified stopped. If any of this is unclear, stop and report the uncertainty. Do not guess.

## No automatic repair

Debug observes. Do not repair a result semantically, rewrite a plan, or retry from here. Where the runtime normalizes a result format, that step is deterministic and says nothing about whether the content is correct. Revisions to unfinished work go through `irrigator propose`, handled by the resume skill.

## Report

1. Run directory, binary version, time of observation.
2. Control state, node counts, reported cost with the caveat that interrupted calls can add unreported charges.
3. Nodes needing attention with `classification`, `error_code`, attempt, and the skill that owns the next step.
4. What you did not observe: the graph was not printed, logs after a sequence were not read, the lease probe was incomplete.

No remote messages, pushes or publishing from this skill.
