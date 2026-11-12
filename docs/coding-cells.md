# Coding cells

[`examples/coder`](../examples/coder) is a supported example of a command worker
that runs a coding agent (Claude Code) in its own git worktree. Each cell lands
as a branch you review with [`irrigator review`](local-review.md). It follows the
[worker protocol](provider-protocol.md) and uses only the Python standard library.

## What a cell does

- Branch `irrigator/<run8>/<cell>`, where `<run8>` is the first eight characters
  of the run ID. A retry continues the cell's existing branch.
- One worktree per cell and attempt, at `<root>/<run8>/<cell>-a<attempt>`. The
  default root is `irrigator-worktrees` beside the repository; override it with
  `--worktrees`. A worktree keeps concurrent checkouts apart; it is not a host
  security sandbox, and the agent runs with your permissions.
- The request's `max_cost_usd` is passed to `claude --max-budget-usd`, so the
  agent checks the cap as it spends. It is an admission-time limit, not a
  guarantee about the final invoice: a provider can overrun it.
- The agent may edit files and run the tool allowlist below. `git push`,
  `reset`, `checkout`, `switch`, `rm`, `WebFetch` and `WebSearch` are denied.
- Changes the agent left uncommitted are committed so the branch holds the work.
- Progress lines go to stderr and, when a controlling terminal exists, to
  `/dev/tty`. Under `IRRIGATOR_EXECUTOR=herdr` that makes each cell's pane show
  what the agent is doing. Pass `--no-tty` to turn the terminal output off.

Outcomes are reported honestly:

| Situation | Outcome |
| --- | --- |
| Validator request, git/worktree setup failed, or `claude` could not be launched | `not_started` |
| Agent timed out, ended without a result, or git failed after it ran | `uncertain` |
| Agent finished (including an agent-reported error, a usage limit, no changes, or failed verification) | `settled`, with `error` set for all but a clean finish |

`uncertain` is never replayed automatically. Inspect the worktree and branch
before deciding to retry.

## Setup

You need `git`, Python 3, and the `claude` CLI logged in on the same machine.
The repository must contain the base branch. From the Irrigator checkout:

```sh
python3 examples/coder/plan.py --repo /path/to/repo --base main --model sonnet \
  --task 'docs=Fix the typos in README.md' \
  --task 'tests=Add a unit test for parse()' > coding.json
```

`--depends tests=docs` runs a cell after another and shows it that cell's
summary. The plan embeds absolute Python and worker paths; regenerate it if they
move, and never edit an admitted plan.

Options for `plan.py` (each is passed through to the worker or the request):

- `--verify ID=COMMAND`: see [Verification](#verification-commands).
- `--add-dir DIR` (repeatable): see [Reference directories](#reference-directories).
- `--continue-from RUNID`: see [Continuing an earlier run](#continuing-an-earlier-run).

## Run it

```sh
./irrigator plan coding.json
./irrigator apply coding.json --run runs/coding-1 --backend coder \
  --workers 2 --timeout 30m --call-budget 2
```

The call budget becomes each cell's `max_cost_usd`. Real model calls spend real
money: choose a budget deliberately.

## Review results

```sh
./irrigator review list --workspace /path/to/repo --base main
./irrigator review change irrigator/<run8>/docs --workspace /path/to/repo --base main
./irrigator review approve irrigator/<run8>/docs --workspace /path/to/repo --base main
```

Each settled cell returns `branch`, `commit`, `diffstat` and `summary`. Merging
is your decision; the worker never pushes.

## Provider environment

Claude Code on macOS finds its login in the Keychain using the `USER`
environment variable. Irrigator passes only `PATH`, `HOME` and the variables a
provider lists in `env`, so the generated plan sets `"env": ["USER"]`. Do not
also pass `LOGNAME`: with both set, Claude Code fails to find the login. If a
cell reports a not-logged-in agent error, check this first.

## Tool allowlist

Besides `Read`, `Edit`, `Write`, `Glob`, `Grep` and `TodoWrite`, the agent may run
these `Bash` commands, so it can build and run tests:

- Build and test: `go`, `gofmt`, `make`, `python3`, `node`, `npm`, `npx`.
- Git: `status`, `diff`, `show`, `log`, `add`, `commit`, `mv`.
- Read-only shell: `cat`, `head`, `tail`, `wc`, `grep`, `find`, `diff`, `ls`.
- File changes: `mkdir`, `cp`, `mv`, `chmod`.

Denied: `git push`, `git reset`, `git checkout`, `git switch`, `rm`, `WebFetch`,
`WebSearch`. The allowlist is a convenience boundary, not a sandbox: `python3`,
`node` and `npm` can run arbitrary code inside the worktree's permissions.

## Verification commands

A cell whose only check is the agent's own summary can be marked verified when
the agent says it could not finish. In swarm runs the agent reported it was
unable to complete its work, yet the cell settled with a branch and no error.
Give every cell that can have one an objective check.

If the request context has `verify_<cell id>` (with `-` in the id replaced by
`_`), the worker runs that shell command in the cell's worktree once the agent
has finished, but only when the branch has commits and the agent did not error:

- Non-zero exit: the cell settles with `error` set to
  `verification failed (exit N): <last 1500 characters of output>` and no output.
- Exit 0: the summary gains `Verification `<command>` passed.`
- A command that outlives the cell's timeout makes the outcome `uncertain`.

`plan.py` attaches the command for you; the value goes in the plan's `context`
and only the named cell receives it:

```sh
python3 examples/coder/plan.py --repo /path/to/repo \
  --task 'tests=Add a unit test for parse()' \
  --verify 'tests=python3 -m unittest discover'
```

## Reference directories

`--add-dir DIR` (repeatable, on `worker.py` or `plan.py`) is passed to `claude`
as `--add-dir`, so agents can read reference material outside the worktree.

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

Every cell is a real `claude -p` run. The call budget you pass to `apply`
becomes each cell's `max_cost_usd` and `--max-budget-usd`, so the worst case for
a run is roughly cells × budget, and each retry is a new run with its own cap.
The reported `cost_usd`, `usage` tokens, and `model` come from the agent's final
stream-json result; a cell that timed out or ended without a result may have spent
money that is not reported. Use `irrigator cost RUN [--json]` to inspect the
per-cell cost statement and run totals (reported spend, unknown-cost operations,
and token totals). Start with a small budget and few workers.

## Cleanup

Worktrees and branches persist after a run. `cleanup.py` lists and removes a
run's worktrees (it matches branches `irrigator/<run8>/*`):

```sh
python3 examples/coder/cleanup.py --repo /path/to/repo --run <run id> --dry-run
python3 examples/coder/cleanup.py --repo /path/to/repo --run <run id>
python3 examples/coder/cleanup.py --repo /path/to/repo --run <run id> --delete-branches
```

- It keeps branches unless you pass `--delete-branches` (`git branch -D`, so
  merge or discard first). A branch whose worktree was not removed is kept.
- It refuses worktrees with uncommitted work, prints them, and exits non-zero,
  unless you pass `--force`, which discards that work. Look before forcing,
  especially for `uncertain` cells.
- `--dry-run` only lists.

By hand: `git worktree remove <path>`, `git branch -D <branch>`,
`git worktree prune`.

`irrigator gc` never touches these. It only removes obsolete attempt logs inside a
valid, quiescent run; it does not reclaim worktrees, delete branches, or infer
worker liveness from a stored controller PID. Worktrees, branches and any dirty
content stay in place for the explicit recovery steps above.

## Verification

`python3 -I tools/test_coder_worker.py` runs the worker, `plan.py` and `cleanup.py`
against a fake `claude` on `PATH` (success, leftovers, agent error, timeout,
missing result, setup failure, missing binary, retries, dirty-worktree refusal,
previous-attempts context, passing and failing verification, the allowlist,
`--add-dir`, `--continue-from`, the usage-limit message, cleanup). It uses no network and no model. This page
records how to run it, not that it passed.

To run a command provider inside a container, see [Container sandbox](sandbox.md).
