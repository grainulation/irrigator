# Use Irrigator with your agent

Irrigator works with the coding agent you already use. Claude Code, Codex and
Antigravity get a full plugin: the workflow skills, plus `irrigator mcp` where the host loads it.
Any other agent that speaks MCP, such as Cursor or opencode, runs `irrigator mcp` directly.

Every option drives the local `irrigator` executable, so install it first and
make sure it is on `PATH` (`irrigator version`). From a checkout:

```sh
make install PREFIX="$HOME/.local"   # then add $HOME/.local/bin to PATH
```

| Agent | Install | What you get |
| --- | --- | --- |
| [Claude Code](#claude-code) | Local marketplace from a checkout | Skills and `irrigator mcp` |
| [Codex](#codex) | Local plugin from a checkout | Skills (MCP: see [Codex setup](codex-plugin.md)) |
| [Antigravity](#antigravity) | `agy plugin install https://github.com/grainulation/irrigator` | Skills and `irrigator mcp` |
| [Other agents](#other-agents) | Add `irrigator mcp` to the agent's MCP settings | MCP tools only |

The MCP server registered by the plugins runs without `--allow-control`: agents
can list and inspect runs, logs and review queues, and validate plans, but not
start or change runs. See [MCP server](mcp.md) to enable control.

## Claude Code

Follow [Claude Code installation](claude-plugin.md). It adds this repository as
a local marketplace and installs the `irrigator` plugin from it.

## Codex

Follow [Codex setup](codex-plugin.md). Codex loads the shared `plugin.json` and
the `.codex-plugin/` overlay from a local checkout.

## Antigravity

Run this in your terminal:

```sh
agy plugin install https://github.com/grainulation/irrigator
```

This installs Irrigator's 9 skills (`irrigator`, `plan`, `run`, `swarm`,
`observe`, `resume`, `budget`, `debug`, `router`) and registers `irrigator mcp`.
Start a new Antigravity session from your project folder.

Antigravity replaced Gemini CLI; install the plugin as above rather than
importing an old Gemini setup. To update, run the install command again; to
remove it, run `agy plugin uninstall irrigator`.

## Other agents

Any agent that supports MCP (Cursor, opencode, Claude Desktop and others) can run
`irrigator mcp` as a stdio server. [MCP server](mcp.md) has the settings snippet
for each host and the full tool list. `irrigator mcp` gives the agent Irrigator's
tools; the workflow skills come with the Claude Code, Codex and Antigravity plugins.

## Check that it works

In a new session, ask your agent:

```text
Use Irrigator to list my runs.
```

A working install answers from the `irrigator` MCP server or CLI. With no runs
yet, that is an empty list.
