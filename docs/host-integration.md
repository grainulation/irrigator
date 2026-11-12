# Host integration

Irrigator is a Markdown skills plugin backed by a separate local runtime.
Installing skills does not start a run, authorize spending, or turn host-owned
agents into Irrigator tasks. Build or supply the CLI first, then confirm that the
host can execute `irrigator version` and `irrigator doctor`.

Use the [Claude Code guide](claude-plugin.md) or [Codex guide](codex-plugin.md) for
installation. A source checkout is available at
[grainulation/irrigator](https://github.com/grainulation/irrigator); a repository
URL does not establish a listing in a host's public plugin directory.

## Terminal host sessions

Launch the installed `claude`, `codex`, `gemini` or `agy` CLI directly from your
terminal, and use its supported Irrigator plugin, skills or MCP integration.
Global host configuration and credentials apply. Hosts decide when to invoke
workflows; ordinary host Agent-tool children do not become Irrigator runtime nodes.

## Check installation and discovery without model calls

From the source root, with supported Codex and Claude CLIs installed:

```sh
python3 -I tools/host-plugin-probe.py --out /absolute/new/evidence-directory
```

The probe stages manifests and skills, uses an isolated Codex configuration to
install and inspect the plugin, and checks Claude's session-only `--plugin-dir`
loading and manifest validation. Temporary installations are removed on exit.
It does not copy credentials or change the user's usual plugin configuration.
Inspect its exit status and generated evidence for your installed host versions;
this procedure is not a promise that every host version supports the same APIs.

Installation, discovery, model invocation, and UI presentation are separate
checks. An invocation check should record the host's skill-loading event, actual
runtime commands, and resulting diagnosis. A model saying it used a skill is not
sufficient evidence. Model use remains subject to the authorized account and
spending scope.

## Observe host-owned Claude agents separately

The terminal dashboard and [MCP server](mcp.md) follow Irrigator run checkpoints. Claude Agent-tool children have a
different identity and lifecycle. Do not manufacture Irrigator run entries for
them or translate their nesting into recorded runtime turns.

`tools/host-agent-events.py` provides an opt-in metadata bridge for Claude's
[SubagentStart and SubagentStop hooks](https://code.claude.com/docs/en/hooks#subagentstart).
It retains host/session/agent identity, agent type, lifecycle event, and receipt
time. It drops prompts, responses, working directories, transcript paths, and
other fields; it neither reads transcripts nor contacts a server.

```sh
mkdir -m 700 /absolute/new/host-events
python3 -I tools/host-agent-events.py settings --directory /absolute/new/host-events > /absolute/new/session-hooks.json
claude --plugin-dir /absolute/path/to/irrigator --settings /absolute/new/session-hooks.json
python3 -I tools/host-agent-events.py status --directory /absolute/new/host-events
```

Use real paths without symlink ancestors. The capture directory must be owned
and private. The bridge bounds input, rejects duplicate fields, and publishes
complete private records atomically. The reader caps directory entries at
10,000; choose a fresh directory for a new capture period. Close the configured
session to stop capture. No persistent hooks are installed automatically.

Records are explicitly `host_reported` and `runtime_task:false`. A stop event is
not verification of the child's work, and a missing stop may represent a stale
capture. These events are not rendered as runtime tasks in any Irrigator view.
The Claude hook schema does not establish equivalent Codex agent observation.
