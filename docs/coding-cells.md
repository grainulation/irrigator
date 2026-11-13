# Coding cells

`irrigator code` runs coding agents on your repository, one task per cell, each
in its own git worktree. Each cell lands as a branch you review with
[`irrigator review`](local-review.md). The agent can be Claude Code (`claude`,
the default), Codex (`codex`) or Antigravity (`agy`).

```sh
irrigator code --repo /path/to/repo --task 'docs=Fix the typos in README.md' \
  --task 'tests=Add a unit test for parse()' --verify 'tests=go test ./...'
irrigator review list --workspace /path/to/repo --base main
```

`irrigator code` builds a plan of cells, writes it next to the run
(`runs/code-<time>.plan.json`, or `--out FILE`) and applies it like
`irrigator apply`. `--plan-only` prints the plan instead of running it. The cells
run through a worker built into the `irrigator` executable, which follows the
[worker protocol](provider-protocol.md).

## What a cell does

- Branch `irrigator/<run8>/<cell>`, where `<run8>` is the first eight characters
  of the run ID. A retry continues the cell's existing branch.
- One worktree per cell and attempt, at `<root>/<run8>/<cell>-a<attempt>`. The
  default root is `irrigator-worktrees` beside the repository; override it with
  `--worktrees`. A worktree keeps concurrent checkouts apart; it is not a host
  security sandbox, and the agent runs with your permissions.
- The agent may edit files and run commands, except the denied ones (see
  [Commands](#commands)).
- Changes the agent left uncommitted are committed so the branch holds the work.
- Progress lines go to stderr and, when a controlling terminal exists, to
  `/dev/tty`. Under `IRRIGATOR_EXECUTOR=herdr` that makes each cell's pane show
  what the agent is doing.

Outcomes are reported honestly, for every agent:

| Situation | Outcome |
| --- | --- |
| Validator request, git/worktree setup failed, the agent could not be launched, or it rejected the run before starting (for example an unknown model) | `not_started` |
| Agent timed out (including agy's own time limit or an interruption), ended without a result, or git failed after it ran | `uncertain` |
| Agent finished (including an agent-reported error, a usage limit, no changes, or failed verification) | `settled`, with `error` set for all but a clean finish |

`uncertain` is never replayed automatically. Inspect the worktree and branch
before deciding to retry.

## Setup

You need `git`, the `irrigator` executable and the agent's CLI, signed in on the
same machine. The repository must contain the base branch.

| Agent | `--agent` | Needs | Default model |
| --- | --- | --- | --- |
| Claude Code | `claude` | `claude` CLI | `sonnet` |
| Codex | `codex` | `codex` CLI | Codex's configured default |
| Antigravity | `agy` | `agy` CLI | agy's configured default |

## Options

```sh
irrigator code --repo DIR --task ID=PROMPT [--task ID=PROMPT ...]
  [--agent claude|codex|agy] [--model M] [--base main]
  [--verify ID=COMMAND] [--depends ID=DEP[,DEP]] [--add-dir DIR]
  [--continue-from RUNID] [--worktrees DIR] [--name NAME]
  [--run DIR] [--workers 2] [--timeout 30m] [--call-budget 1]
  [--out FILE] [--plan-only]
```

- `--task ID=PROMPT`: one cell. IDs use letters, digits, `_` and `-`.
- `--depends tests=docs`: run `tests` after `docs` and show it `docs`'s summary.
- `--verify`, `--add-dir`, `--continue-from`: see the sections below.
- `--workers`, `--timeout` and `--call-budget` are passed to `apply`. The plan
  embeds the absolute path of the `irrigator` executable and the repository;
  regenerate it if they move, and never edit an admitted plan.

Real model calls spend real money or account quota: choose the budget, timeout and
number of workers deliberately.

## Review results

```sh
irrigator review list --workspace /path/to/repo --base main
irrigator review change irrigator/<run8>/docs --workspace /path/to/repo --base main
irrigator review approve irrigator/<run8>/docs --workspace /path/to/repo --base main
```

Each settled cell returns `branch`, `commit`, `diffstat` and `summary`. Merging
is your decision; the worker never pushes.

## Provider environment

Claude Code on macOS finds its login in the Keychain using the `USER`
environment variable. Irrigator passes only `PATH`, `HOME` and the variables a
provider lists in `env`, so the generated plan sets `"env": ["USER"]`. Do not
also pass `LOGNAME`: with both set, Claude Code fails to find the login. If a
cell reports a not-logged-in agent error, check this first. Codex and agy read
their sign-in from `HOME`.

## Commands

Every agent is told which commands are denied: `git push`, `git reset`,
`git checkout`, `git switch` and `rm` (delete files with `git rm`). How each
agent enforces that differs:

- **Claude** runs with `--permission-mode acceptEdits`, no settings files
  (`--setting-sources ""`, so your own allow rules cannot approve anything for a
  cell) and an allowlist: file tools plus these commands so it can build and
  test: `go`, `gofmt`, `make`, `python3`, `pytest`, `node`, `npm`, `npx`, `cargo`;
  git `status`, `diff`, `show`, `log`, `add`, `commit`, `mv`, `ls-files`, `grep`,
  `rev-parse`; read-only shell (`ls`, `cat`, `head`, `tail`, `wc`, `grep`, `find`,
  `diff`, `sed`, `sort`, `uniq`, `cut`, `tr`, `pwd`, `echo`, `which`, `test`); and
  `mkdir`, `cp`, `mv`, `chmod`. The denied commands, `WebFetch` and `WebSearch`
  are refused.
- **Codex** runs `codex exec` in its own `workspace-write` sandbox with approvals
  off. The sandbox lets it write the worktree and the repository's git directory
  (so it can commit), and blocks network access from commands, so tasks that need
  to download dependencies fail.
- **agy** runs in print mode with session-only settings that deny the commands
  above and keep its file tools inside the worktree; they replace your own agy
  allow and deny rules for the cell, and they also apply inside any subagents agy
  starts (tested). Other commands run without asking: a refused command does not
  end agy's turn by itself, but the refusal tells the model not to work around it
  and it often stops there, so an allowlist would stop cells on harmless commands.
  Subagents' own steps do not appear in the cell's progress; their start does.

These are convenience guardrails, not a sandbox (Codex's sandbox aside):
`python3`, `node` and similar commands can do anything your user can, including
what the deny list names. In testing, agy deleted a cache directory through
`python3` after being told not to use `rm`.

## Verification commands

A cell whose only check is the agent's own summary can be marked verified when
the agent says it could not finish. Give every cell that can have one an
objective check.

`--verify ID=COMMAND` puts the command in the plan's `context` as
`verify_<cell id>` (with `-` replaced by `_`); only that cell receives it. Once
the agent has finished, and only when the branch has commits and the agent did
not error, the worker runs it with `/bin/sh -c` in the cell's worktree:

- Non-zero exit: the cell settles with `error` set to
  `verification failed (exit N): <last 1500 characters of output>` and no output.
- Exit 0: the summary gains ``Verification `<command>` passed.``
- A command that outlives the cell's timeout makes the outcome `uncertain`.

## Reference directories

`--add-dir DIR` (repeatable) lets agents read reference material outside the
worktree. It is passed to each agent's own `--add-dir`.

## Continuing an earlier run

`--continue-from RUNID` lets a new run build on an earlier one. When a cell's
branch does not exist yet and `irrigator/<RUNID[:8]>/<cell>` does, the new branch
is created from it instead of from the base, and the `previous_attempts` note
lists that work. If the cell's branch already exists, the flag is ignored and the
normal retry rules apply. If the earlier branch is missing, the cell starts from
the base.

## Usage limits

When the agent's result says the account hit a session or usage limit, the cell
settles with an error that says so and includes the reset time text the agent
gave (for example `resets 3pm`), instead of a generic `agent error`. That is not
a code problem: resume the cell after the reset rather than debugging.

## Retries

When a cell's branch already exists, the worker adds the new attempt's worktree
on that branch instead of starting a new one:

- Earlier attempts' worktrees that hold the branch are removed with
  `git worktree remove`, but only if they are clean. If one has uncommitted
  work, the retry returns `not_started` with an error naming the worktree path,
  and nothing is touched. Commit or discard that work, then retry.
- If the branch already has commits beyond the base, the prompt gains a
  `previous_attempts` context section that lists them and tells the agent to
  review and continue that work rather than start over.

A retry of an `uncertain` cell is still your decision; Irrigator never replays
interrupted work automatically. Look at the earlier worktree first.

## Watch a worker

Under `IRRIGATOR_EXECUTOR=herdr` each worker runs in its own pane and shows the
progress lines described above. From another terminal:

```sh
hc read <pane>     # print the pane's recent output without attaching
hc attach <pane>   # attach to the live pane
```

Leave an attached pane with **Ctrl+\\**, which detaches and leaves the worker
running. **Ctrl+C stops the agent**: it is delivered to the process in the pane,
so the cell will end `uncertain` (or `not_started` if it had not launched yet).

## Costs

- **Claude** reports each cell's USD cost. The call budget becomes the cell's
  `max_cost_usd` and `claude --max-budget-usd`, so the agent checks the cap as it
  spends; the worst case for a run is roughly cells × budget, and each retry is a
  new run with its own cap. It is an admission-time limit, not a guarantee about
  the final invoice.
- **Codex** and **agy** report token counts but no USD cost, and accept no USD
  cap: their cells record cost as unknown (never $0) with the token counts, and
  the cell timeout is the hard limit.

A cell that timed out or ended without a result may have spent money that is not
reported. `irrigator cost RUN [--json]` shows the per-cell cost statement and
run totals (reported spend, unknown-cost operations and token totals). Start with
a small budget and few workers.

## Cleanup

Worktrees and branches persist after a run. `irrigator code cleanup` lists and
removes one run's worktrees (it matches branches `irrigator/<run8>/*`):

```sh
irrigator code cleanup --repo /path/to/repo --run <run id> --dry-run
irrigator code cleanup --repo /path/to/repo --run <run id>
irrigator code cleanup --repo /path/to/repo --run <run id> --delete-branches
```

- It keeps branches unless you pass `--delete-branches` (`git branch -D`, so
  merge or discard first). A branch whose worktree was not removed is kept.
- It refuses worktrees with uncommitted work, prints them, and exits non-zero,
  unless you pass `--force`, which discards that work. Look before forcing,
  especially for `uncertain` cells.
- `--dry-run` only lists.

By hand: `git worktree remove <path>`, `git branch -D <branch>`,
`git worktree prune`.

`irrigator gc` and `irrigator cleanup` never touch these. `gc` only removes
obsolete attempt logs inside a valid, quiescent run; it does not reclaim
worktrees, delete branches, or infer worker liveness from a stored controller
PID. Worktrees, branches and any dirty content stay in place for the explicit
recovery steps above.

## Verification

`go test ./internal/coder` runs the worker, the plan builder and cleanup against
fake `claude`, `codex` and `agy` executables on `PATH` that emit each agent's
stream format (success, leftovers, agent error, usage limit, timeout, missing
result, no changes, setup failure, missing binary, retries, dirty-worktree
refusal, previous-attempts context, passing and failing verification,
`--add-dir`, `--continue-from`, cleanup). It uses no network and no model.

For agents over ACP, with Irrigator's approval links gating each tool call, see
the [ACP worker](acp-worker.md). To run a command provider inside a container,
see [Container sandbox](sandbox.md).
