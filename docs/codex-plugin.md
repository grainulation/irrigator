# Irrigator for Codex

The plugin adds Markdown workflows around the local `irrigator` executable.
It declares no MCP server and no additional permissions: the Codex manifests
documented here have no place for one. Installing it does not install the
runtime, authorize paid work, or execute a task graph. To use the read-only
`irrigator mcp` server ([MCP server](mcp.md)), see [MCP](#mcp) below.

## Package layout

| Path | Role |
| --- | --- |
| `plugin.json` | Portable root manifest; OpenAI-specific presentation and `requires_local_executor` are under `extensions.com.openai` |
| `.codex-plugin/plugin.json` | Compatibility manifest with a flat `interface`, `requires_local_executor`, and `skills: "./skills/"` |
| `skills/` | Nine Markdown workflows: `irrigator`, `router`, `swarm`, `plan`, `run`, `resume`, `observe`, `budget`, and `debug` |

Paths resolve from the plugin root. The two manifests have different layouts;
do not replace one with a byte-for-byte copy of the other. The local-executor
flag describes a runtime requirement, not a permission grant. These placements
follow the [official packaging guide](https://developers.openai.com/plugins/build/plugins)
and [local-execution requirement](https://developers.openai.com/plugins/deploy/desktop-only).

The project is [MIT licensed](../LICENSE) and maintained at
[grainulation/irrigator](https://github.com/grainulation/irrigator). This source
layout does not assert a public plugin-directory listing or host endorsement.

## Build and load locally

Use Go 1.26 or newer and a POSIX shell for the complete runtime:

```sh
make build
./irrigator version
./irrigator doctor
make smoke
```

Optionally install the binary explicitly with `make install`, or keep using an
absolute executable path. Then use the local-plugin or local-marketplace mechanism
supported by your Codex version, following its documentation and permission
prompts. Point it at the root containing `plugin.json`, `.codex-plugin/`, and
`skills/`. Local marketplaces are separate from the universal public directory;
see the [official local marketplace guidance](https://developers.openai.com/plugins/build/plugins).

For an isolated installation/discovery probe, see [host integration](host-integration.md).
Check the resulting evidence for your installed host rather than inferring
successful loading from manifest files alone.

## Manual workflow

If the host does not load the plugin, read `skills/irrigator/SKILL.md` or
`skills/router/SKILL.md` and follow the selected workflow as plain Markdown.
This fallback does not require a Claude-specific `Skill` tool or a host alias.

| Need | Skill | Runtime commands |
| --- | --- | --- |
| Orient | `irrigator`, `router` | `version`, `doctor` |
| Draft a graph | `plan` | `init`, `plan`, `size` |
| Execute and deliver | `run` | `prepare`, `apply`, `deliver`, `export` |
| Recover | `resume` | `status`, `inspect`, `resume`, `retry`, `resolve`, `propose` |
| Observe or diagnose | `observe`, `debug` | `status`, `graph`, `logs`, `debug` |
| Bound spending | `budget` | `size`, provider reservations, managed execution profiles |

Use the installed CLI's help and versioned contracts as the authority if a skill
and executable disagree. Shapes do not authorize spending; worker concurrency is
independent of graph capacity. Preserve accepted outputs and the admitted plan.
Use supported revisions, and never automatically replay an uncertain call.

## MCP

Register the server yourself in `~/.codex/config.toml` (one entry, read-only; the
`irrigator` executable must be on `PATH`, or use its absolute path):

```toml
[mcp_servers.irrigator]
command = "irrigator"
args = ["mcp"]
```

Add `"--allow-control"` to `args` only if you want Codex to start, pause, resume
or decide approvals on runs; the plugin never enables it. Add
`"--runs", "/absolute/path/to/runs"` to list runs by name. The
`skills/irrigator/SKILL.md` workflow shows the same entry. Everything the server
offers is also available through the CLI.

## Executors, sandbox and approvals

The skills mention three newer features that need no plugin support: worker
executors (`--executor local|herdr|tmux`, availability shown by
`irrigator doctor`; see [executors](executors.md)), an optional container
`sandbox` for command providers (a convenience boundary, not a security
boundary; see [sandbox](sandbox.md)), and `irrigator approvals`
(see [approvals](approvals.md)).

## Permission and verification boundaries

Irrigator runs trusted local processes; task cells are not sandboxes unless a
provider configures the optional container sandbox, which is a convenience
boundary only. Codex's
approval, filesystem, and network restrictions remain in force for CLI commands,
run directories, loopback servers, and providers. Presentation capabilities such
as `Read` and `Write` do not grant access.

Plugin discovery is separate from model invocation, and invocation is separate
from accepted runtime work. Run mock checks without credentials first. Real
provider calls require an authorized account and spending scope; see
[execution profiles](execution-profiles.md) and [platform support](platform-support.md).
