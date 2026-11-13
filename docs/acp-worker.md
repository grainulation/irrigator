# ACP command worker

`examples/acp/worker.mjs` is a `swarm.worker/v1` command worker that drives a
coding agent over the [Agent Client Protocol](https://agentclientprotocol.com)
(ACP) and puts every approval request in front of a person. It is a Node example
(ESM, no build step) that uses the official client SDK,
`@agentclientprotocol/sdk` 2.4.0, and spawns `@agentclientprotocol/claude-agent-acp`
0.101.0 from `node_modules`. Both versions are pinned in `package.json` and
`package-lock.json`.

```sh
cd examples/acp && npm ci --no-audit --no-fund
npm test
```

Use it like the built-in [coding worker](coding-cells.md) behind `irrigator code`: a command worker that takes the
request on stdin and prints one `swarm.worker/v1` response.

```sh
node examples/acp/worker.mjs --repo /path/to/repo --base main
node examples/acp/worker.mjs --repo /path/to/repo --base main --agent agy
node examples/acp/worker.mjs --repo /path/to/repo --base main --agent gemini
```

By default `--agent` launches `@agentclientprotocol/claude-agent-acp` from
`node_modules`. Passing `--agent agy` launches `agy acp` (ACP v1 over stdio) and
passing `--agent gemini` launches `gemini --acp` (ACP v1 over stdio); custom
arguments can be passed with repeatable `--agent-arg <arg>`.

## What it does

- **Worktree per cell.** Same layout as [`irrigator code`](coding-cells.md): branch
  `irrigator/<run8>/<cell>`, worktree `<worktrees>/<run8>/<cell>-a<attempt>`
  (default root `irrigator-worktrees` beside the repo). A retry continues the
  cell's branch. Leftover changes are committed, and the result carries `branch`,
  `commit`, `diffstat` and `summary`, ready for `irrigator review list`.
- **Ask-first mode.** After `session/new` the worker selects the `default` mode
  (the agent also offers `acceptEdits`, `plan`, `auto`, `bypassPermissions`). If
  the agent offers modes but not `default`, nothing is dispatched.
- **Same denied commands.** `git push`, `git reset`, `git checkout`, `git switch`,
  `rm`, `WebFetch` and `WebSearch` are rejected immediately, without asking
  anyone, in every shell segment (`cd x && git push` is caught).
- **Everything else needs a person.** On `session/request_permission` the worker:
  1. mints two links with `irrigator approvals link --run <run root> --request
     <task id> --action approve|deny` (the run root is two directories above the
     worker's cwd; override with `--run-root` or `IRRIGATOR_RUN_ROOT`, and the
     binary with `--irrigator` or `IRRIGATOR_BIN`);
  2. if `IRRIGATOR_NTFY_TOPIC` and `IRRIGATOR_APPROVALS_BASE_URL` are set, posts
     a notification (title `Irrigator approval: <cell>`, body the tool title)
     with Approve and Deny buttons that POST to the base URL plus the link
     (`IRRIGATOR_NTFY_SERVER` defaults to `https://ntfy.sh`);
     When the run has a notifier (`IRRIGATOR_NOTIFY_VIA` or `RUN/notify.json`, see
     [monitoring.md](monitoring.md#notifications-for-unattended-runs)), the worker
     instead calls `irrigator approvals notify --json` and waits on the nonces it
     returns; if that fails it falls back to plain links;
  3. waits for a decision in `<run root>/approvals/decisions.jsonl`, matching the
     record's `request` and the `nonce` of the two links it just minted, so a
     decision for another request or an older prompt is never reused;
  4. answers `allow_once` for approve and `reject_once` for deny.

  No decision within `--permission-timeout` seconds (default 300, or
  `IRRIGATOR_ACP_PERMISSION_TIMEOUT_S`) rejects. So does a failure to mint links,
  an approval when the agent offered no `allow_once`, and a cancelled request.
  `allow_always` is never chosen. A deny wins over an approve if both exist.
  Links and tokens are never logged or put in argv beyond the `irrigator` call.

Start `irrigator approvals serve` for the run (see [approvals.md](approvals.md))
and expose it as described there so the buttons can reach it.

## Outcomes and cost

| Outcome | When |
| --- | --- |
| `not_started` | request is not a task, git setup failed, the agent could not be launched, initialize/`session/new` failed, or no ask-first mode exists |
| `uncertain` | timeout, the agent crashed or the connection closed after the prompt was sent, the prompt returned an error, or git failed after the agent ran |
| `settled` | the agent stopped. `end_turn` with commits is success; no commits, or another stop reason (`refusal`, `max_tokens`, `cancelled`, ...), settles with `error` |

`cost_usd` is the cumulative cost from the last `usage_update` notification, only
when its currency is `USD`. `usage` and `model` are forwarded from the ACP prompt
result in canonical `swarm.worker/v1` format. When an agent reports token usage
without USD cost (such as `agy`), Irrigator records the cost basis as `unknown`
and preserves the token measurement rather than displaying $0. It is reported on
every outcome, including `uncertain`. As a soft guard the worker sends `session/cancel` once that cost
exceeds the request's `max_cost_usd` and settles with an error; the cost is only
as fresh as the agent's last update.

## Limits

- **Not a managed worker.** ACP is for interactive, approval-driven cells.
  The agent reports its own cost after the fact; there is no budget reservation
  or receipt, so this worker does not replace the managed workers' budget
  receipts. Use it where a person is in the loop, not for unattended spend.
- **Ledger runs only.** The decision is read from `approvals/decisions.jsonl`,
  which `approvals serve` writes for runs without the durable mailbox. For a
  mailbox-enabled plan the decision becomes a mailbox `signal` instead; use a ledger run for ACP worker approval decisions.
- A crash or timeout never replays: the cell is `uncertain` and the worktree is
  left for inspection.
- Tests use a fake ACP agent and a fake `irrigator`; no network or model is used.

## Agents other than Claude

Agent-specific behavior (command resolution, the agy minimum version check and
ask-first settings, and the tool kinds that must be put to a person) lives in
`examples/acp/adapters/`. `worker.mjs` imports it and re-exports the
names (`AGY_MIN_VERSION`, `AGY_SETTINGS`, `parseSemver`, `semverAtLeast`,
`checkAgyVersion`, `GATED_KINDS`), ensuring compatibility between the worker and the adapter.

**agy.** The worker starts `agy acp` with its own settings directory
(`--gemini-dir`, removed afterwards) set to ask before every tool call. A
global `~/.gemini/antigravity-cli/settings.json` with `"toolPermission":
"always-proceed"` or a `permissions.allow` list would otherwise let agy run
commands and edits without sending a permission request, which bypasses the
deny list and approval links. Passing your own `--agent-arg` opts out of this
isolation. The isolated directory still uses your agy sign-in.

**Claude.** The bundled Claude adapter starts each session with no user,
project or local settings files (`settingSources: []`), so your own allow rules,
hooks and permission mode cannot approve anything for a cell; sign-in is
unchanged. That also drops what those files provide: hooks (for example
format-on-edit), settings-based `env` routing or `apiKeyHelper` (a cell then
fails to start, it never runs on other credentials), plugins and MCP servers
enabled in settings, your default `model` (pass `--model` to choose one for the
cell), and the repository's `CLAUDE.md`, which the cell prompt tells the agent
to read instead. The Claude coder worker passes
`--setting-sources ""` for the same reason; the Claude CLI still loads
`CLAUDE.md` in that mode.

**Unapproved calls.** An execute, edit, delete, move or fetch call that completes
without a permission request stops the cell: the worker cancels the session and
reports the cell as `uncertain`, because the change is real but was never
approved. For the Claude adapter shell commands are exempt: in the ask-first
mode Claude runs read-only commands such as `ls` without asking and asks before
commands that change files.

**Model.** `--model ID` picks the cell's model. For the Claude adapter it is
passed as a session option; for other agents the worker sets the ACP `model`
config option after creating the session (agy: for example `gemini-3.8-flash` or
`claude-sonnet-4-6`). A model the agent does not offer leaves the cell
not_started with the list of offered values.

**Cost.** agy sends no `usage_update` cost, only token counts, so the soft
budget cannot stop an agy cell; the worker says so in its log and reports the
cost as unknown, never as zero. agy returns token usage only when the turn ends,
so the worker adds it to the log and the cell summary (for example `15610 tokens
(9782 in, 513 out, 233 thinking, 5315 cached)`); the cell timeout remains the hard
limit.

