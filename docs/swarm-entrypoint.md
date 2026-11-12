# Swarm entrypoint

`skills/swarm/SKILL.md` is a swarm-first front door for the Irrigator plugin. It reads a request, picks `plan`, `budget`, `run`, `resume` or `observe`, and hands off to that skill. It is Markdown only: no CLI command, manifest field, command file or MCP server was added. The `irrigator` binary has no `swarm` subcommand.

Both plugin packages ship nine skills: `irrigator`, `router`, `plan`, `run`,
`resume`, `observe`, `budget`, `swarm`, and `debug`. The packaging and contract
checks include all nine; host discovery checks are described in
[host integration](host-integration.md).

## Invoking it, by host

The skill's name is `swarm` and the plugin's name is `irrigator`. How a user writes that depends on the host. This plugin does not ship an alias, and it does not promise one `/swarm` spelling that works everywhere.

| Host | Native form the host documents | Status here |
| --- | --- | --- |
| Claude Code | Plugin skills are namespaced by plugin: `/<plugin-name>:<skill-name>`, so plugin `irrigator` plus skill `swarm`. The host may also load the skill on its own when a request matches its description. | Invoke `/irrigator:swarm` after loading the plugin. Check the host skill list if discovery is disabled. |
| Codex | A skill is invoked explicitly by naming it in the prompt (`$skill-name`, so `$swarm`), or chosen implicitly from its description. Whether a plugin's skills appear, and how, depends on the version. | Select the installed `swarm` skill from the host skill list; use the qualified entry if multiple plugins provide it. |
| Any other host, or a host that does not list the skill | Say it in words: 'use the Irrigator swarm skill', or open `skills/swarm/SKILL.md` and follow it. | Works wherever the agent can read files. |

A bare `/swarm` is not documented here for any host. If your host happens to resolve it, that is the host's behaviour. Another installed plugin or skill may also be named `swarm`, which is a reason to use the qualified form. The `$ARGUMENTS` placeholder in the skill is a host substitution; where it is left as literal text the skill tells the agent to use the user's message.

Implicit selection stays at the host default. No `agents/openai.yaml` or other invocation policy file was added. The description names Irrigator and durable local agent graphs so that unrelated uses of the word swarm are less likely to match.

## What to type after the name

Arguments follow the skill name, however your host spells the invocation.

| Arguments | Goes to |
| --- | --- |
| `plan <task or shape>` | `skills/plan/SKILL.md` |
| `budget <shape or plan>` | `skills/budget/SKILL.md` |
| `run <plan file>` | `skills/run/SKILL.md` |
| `resume <run directory>` | `skills/resume/SKILL.md` |
| `observe <run directory>` | `skills/observe/SKILL.md` |
| Nothing, or free text | Chosen from the evidence below |

A leading word counts as a stage word only when the rest looks like Irrigator input. The aliases design, shape, size, cost, apply, launch, recover, retry, status and watch are conveniences defined by the skill, not CLI commands.

## Routing by evidence

| Given | Start with |
| --- | --- |
| A run directory containing `state.json` | `observe` (read-only). Failed, uncertain, stalled or blocked nodes then go to `resume`; a complete run goes to acceptance and export. |
| A plan file, no run directory | Validate with `irrigator plan PLAN`, then `budget`. `run` only when the user asked to launch. |
| A shape such as `25x5x2`, or a task to split | `plan` |
| A question about agent count or cost | `budget` |
| Nothing usable | One short question. |

When more than one row fits, `skills/router/SKILL.md` and its earliest-stage rule decide. The router and `swarm` do not call each other in a loop: `swarm` defers to the router table, and the router tells an agent that arrived from `swarm` to route with the table.

A shape is a ceiling and not a worker count: `25x5x2` allows at most 775 nodes, and `--workers` is chosen separately.

## Spend and authorization

The entrypoint adds no approval gate and removes none. The stage skills own them.

- A shape, the word swarm, or invoking this skill is not an authorization to spend.
- Paid work needs the stated roots, levels, max children, max and intended tasks, `--workers`, per-call limit and aggregate cap, and an explicit yes from the user.
- An authorization already given in the session for that plan, backend and cap carries over to resume, replay-safe retry and export inside it. A new scope or a higher cap needs a fresh statement and a fresh yes.
- `--call-budget` (`--budget` is an alias) is per call, not a graph-wide cap.
  Managed execution profiles support run-level admission reservations; see
  [execution profiles](execution-profiles.md). Neither reservation caps nor a
  provider service allocation guarantee the final external invoice.
- Uncertain nodes are never replayed on general authorization alone.
- Free paths need no authorization: `--backend mock`, `prepare`, `size`, `demo`.

## Discovery and packaging

The `.claude-plugin` and `.codex-plugin` manifests point at the same `skills/`
directory. `tools/package-plugin.py` requires all nine skills, including `swarm`
and `debug`; `verification/plugin-contract-test.py` checks their presence and
routing concepts. The packaged consumer test exercises the installed archive
against the CLI contract.

For installation and host discovery verification, follow
[host integration](host-integration.md). Host versions, plugin configuration, and
safe-mode settings can affect discovery. Successful packaging does not by itself
establish that a particular host installation has loaded the skills.
