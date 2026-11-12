---
name: swarm
description: Swarm-first front door for Irrigator durable local agent graphs. Routes a request, or a leading plan, run, resume, observe or budget word, to the matching Irrigator workflow skill. Not for ordinary single-agent work.
---

# swarm: Irrigator front door

One entry for starting or continuing Irrigator work. It picks the stage and hands off. It does not replace the stage skills and adds no commands of its own. The `irrigator` CLI has no `swarm` subcommand (`swarm/v1` is only the plan format name). Follow this file as plain Markdown and open sibling skills by path.

How the user types the invocation depends on the host. Do not tell the user a bare `/swarm` command exists, and do not claim a host lists this skill, unless you have seen that host do it. `docs/swarm-entrypoint.md` has the host-qualified forms and the manual fallback.

## Arguments

$ARGUMENTS

If the text above is still the literal placeholder, the host did not substitute it. Use the user's message instead.

## Choose the stage

If the request describes sequential thinking turns and terminal one-shot helpers,
read `docs/turn-shapes.md` first. `turn-shape XxYxZ --json` previews capacity;
execution requires a plan with the corresponding turn contract. The scheduler
executes these turns and terminal helpers. Keep X main agents, Y optional turns,
and Z helper shorthand separate from A×D×S ownership trees; a turn is not a
new main-agent identity. The preview command itself does not launch work.

1. **Leading stage word.** If the arguments start with one of these words and the rest looks like Irrigator input (a plan file, run directory, shape or graph description), route there and pass the rest on as that skill's arguments. A word followed by unrelated text, such as 'run the tests', is not a stage word.

| Word (aliases) | Skill file |
| --- | --- |
| `plan` (design, shape) | `skills/plan/SKILL.md` |
| `budget` (size, cost) | `skills/budget/SKILL.md` |
| `run` (apply, launch) | `skills/run/SKILL.md` |
| `resume` (recover, retry) | `skills/resume/SKILL.md` |
| `observe` (status, watch) | `skills/observe/SKILL.md` |
| `debug` (diagnose) | `skills/debug/SKILL.md` (read-only; any change goes to `resume`) |

2. **No stage word: read the evidence you were given.**

| Given | Start with |
| --- | --- |
| A run directory (it holds `state.json`) | `observe`, read-only. Then hand off from what `irrigator status DIR --json` reports: failed, uncertain, stalled or blocked goes to `resume`; complete goes to acceptance and export as described in `run` and `resume` |
| A plan file, no run directory | `irrigator plan PLAN` to validate, then `budget` for the pre-launch statement. Go on to `run` only if the user asked to launch |
| A shape such as `25x5x2`, or a task to split, and no plan file | `plan` |
| A question about agent count or cost | `budget` |
| Nothing usable | Ask one short question: a new graph, or an existing run directory? |

3. **Several matches.** `skills/router/SKILL.md` holds the intent map and the earliest-stage rule. Use it instead of restating it here, and do not hand back to this file from there.

Say which skill you chose and why in one sentence, then open it and follow it. Keep the user's outcome: if they asked for delivered work, plan and run are both in scope and the job ends with accepted artifacts, not a launch. If a sibling file is missing, say so and apply `skills/irrigator/SKILL.md`.

## What this entry does not change

- **Spend.** A shape, the word 'swarm' or this entry is never an authorization. Paid work (`--backend haiku` or a billing provider) follows the gate in `skills/run/SKILL.md` and `skills/budget/SKILL.md`: state roots, levels, max children, max and intended tasks, `--workers`, the per-call limit and the aggregate cap, then get an explicit yes. `--budget` is per call, not a graph-wide cap. An authorization already given this session for that plan, backend and cap stays valid, so resume, replay-safe retry and export inside it need no new yes. A new plan, a new backend or a higher cap does. Without authorization stay free: `--backend mock`, `prepare`, `size` or `demo`.
- **Exact sources.** Read named files from their exact absolute paths. A missing or unreadable input stops the work; never substitute a near match.
- **Uncertain work.** Never replay blindly. `inspect` first, then `resolve`, or retry with `--allow-uncertain` only after the user explicitly accepted possible duplication.
- **Immutable plans.** Unfinished work changes through `irrigator propose`, not by editing an admitted plan.
- **Evidence.** Report dispatch from `irrigator status DIR --json`, `inspect` or `logs`, not from a process saying `running`. Verified is not correct, and a reviewer is not the author.
- **Local and host-respecting.** No push, publish, remote message, build or install unless the user asked. Host permission prompts apply; a declined command is reported as not run. Run `irrigator doctor` (it also lists the `local`, `herdr` and `tmux` executors) and `irrigator version` before saying the runtime works, and say so if the binary is missing. Executors, the provider container `sandbox`, `irrigator approvals` and the optional read-only `irrigator mcp` server are covered in `skills/irrigator/SKILL.md` section 10.

## When not to use

- Ordinary code edits or one-agent tasks that did not ask for an Irrigator graph.
- Questions about how Irrigator works: answer from the docs.
- A request about some other tool's swarm: ask which tool is meant.

## Next steps

End with exactly two bullet lists, **Auto** and **Manual**, as the stage skills do. Auto is work already authorized. Manual is a decision, access or spend authorization only the user can give. Use `None.` for an empty list.
