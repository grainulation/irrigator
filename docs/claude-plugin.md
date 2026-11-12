# Claude Code plugin

Irrigator's plugin supplies Markdown workflows for the local `irrigator` CLI.
It bundles no runtime binary, agents, hooks, or permission grants. It does
register the read-only `irrigator mcp` server ([MCP server](mcp.md)) so the
host can offer it once the plugin is installed; see [MCP](#mcp).
The [MIT license](../LICENSE) applies to the project; the local marketplace file
is not a claim of public marketplace publication.

## Requirements

Build the CLI with Go 1.26 or newer for the complete runtime, or supply a trusted
binary. Make it available on `PATH` or use its absolute path in commands. Skills
alone do not build, download, or install an executable.

```sh
make build
./irrigator version
./irrigator doctor
make smoke
```

`doctor` separates executable discovery from authentication. Mock work needs no
model account; real provider work needs its own configuration and authorization.
See [platform support](platform-support.md) and [execution profiles](execution-profiles.md).

## Load the plugin

For a session without a persistent plugin install:

```sh
claude --plugin-dir /absolute/path/to/irrigator
```

For a persistent local-marketplace installation, run these commands inside Claude
Code, replacing the path with this checkout or an extracted plugin root:

```text
/plugin marketplace add /absolute/path/to/irrigator
/plugin install irrigator@irrigator-local
```

Reload plugins or start a new session as required by your host. Check the installed
host's help if its syntax differs. These installation steps deliberately change
host configuration; building or packaging Irrigator does not perform them.

Use namespaced skills such as `/irrigator:plan` or `/irrigator:debug` only after
the host lists them. A bare `/swarm` alias is not promised. If skills are not
loaded, open the appropriate `skills/*/SKILL.md` and follow it manually.

## MCP

`.claude-plugin/plugin.json` declares one entry, `irrigator`, which
runs `irrigator mcp` (the [MCP server](mcp.md)) from the `irrigator` on `PATH`.
It has no `--runs` directory and no `--allow-control`: it is read-only, and tools
accept an explicit run directory path. The plugin does not install the binary; if
`irrigator` is not on `PATH`, the host reports the server as failed to start and
the skills still work through the CLI. To let the host start, pause, resume or
decide approvals, the user must add the server themselves with `--allow-control`
(see [MCP server](mcp.md)); the plugin never enables it.

## Available workflows

| Skill | Purpose |
| --- | --- |
| `irrigator`, `router`, `swarm` | Orient and choose the appropriate workflow |
| `plan`, `budget` | Prepare an explicit plan, capacity, and spending boundaries |
| `run` | Preflight, execute, accept, and export authorized work |
| `resume` | Inspect and continue interrupted work without blind replay |
| `observe`, `debug` | Inspect run evidence and diagnose issues read-only |

The skills also cover executors (`--executor local|herdr|tmux`, checked with
`irrigator doctor`), the optional container sandbox for command providers, and
`irrigator approvals`; see [executors](executors.md), [sandbox](sandbox.md) and
[approvals](approvals.md).

A×D×S ownership trees and agent/turn/helper shapes are different
contracts; read [swarm shapes](swarm-shapes.md) and [turn shapes](turn-shapes.md).
Worker concurrency and spending limits are separate from either shape.

The host's permission controls still apply. Preserve admitted plans, use
`propose` for unfinished-work revisions, and inspect uncertain external effects
before an explicit retry. Public display metadata requires its own allowlisted
fields and publication opt-in; see [public metadata](public-display-metadata.md).

For configuration-isolated discovery checks and opt-in host-agent capture, see
[host integration](host-integration.md). Manifest validation, skill discovery,
and an actual model invoking a skill establish different things.
