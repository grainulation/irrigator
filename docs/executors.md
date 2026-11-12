# Executors

An executor decides how the controller launches each worker process. The
worker, its cell lease, checkpoints and results are the same under every
executor; only where the process runs differs.

| Executor | Runs the worker | Requirements |
| --- | --- | --- |
| `local` (default) | as a direct child of the controller | none |
| `herdr` (opt-in) | in a herdr-core terminal, started with `hc run` | herdr-core `hc` >= 0.1.19 on `PATH` |
| `tmux` (opt-in) | in its own window of a per-run tmux session | `tmux` >= 3.2 on `PATH` |

`irrigator doctor` lists all three under `executors`: `local` is always
available; `herdr` and `tmux` report their path and version as OK, too old, or
not installed. Not having herdr or tmux is not an error.

## Choosing an executor

`apply`, `prepare`, `resume` and `deliver` accept `--executor local|herdr|tmux`.
Precedence, highest first:

1. `--executor`
2. the executor recorded for the run
3. `IRRIGATOR_EXECUTOR`
4. `local`

The choice made when a run is created is recorded in `<run>/executor.json`.
Runs created before this file existed have none and default to `local`. A later
`resume` keeps the recorded executor, so setting the environment variable
afterwards does not move a run. If `--executor` differs from the recorded
executor it wins, one line saying so is printed, and the record is updated.
Unknown names fail with the list of known executors.

## herdr

[herdr-core](https://herdr.dev/core/) hosts each worker in a terminal that
outlives its client. Install it and check `hc --version` (format `hc 0.1.19`).

**Minimum version.** 0.1.19 is the release whose `hc run --timeout`, labels and
`hc ls --json` behaviour Irrigator relies on (`herdrMinVersion`). An older or
unreadable `hc` fails before any worker is launched, naming the found and
required versions.

**Environment.** `hc` programs inherit herdr-core's environment, not the
controller's, so the worker is started through `env` with a small allowlist:
`PATH`, `NO_COLOR`, `IRRIGATOR_*` and `SWARM_*`, plus worker progress lines
(which give `hc attach` and `hc read` something to show). Credentials are never
forwarded: `hc` shows a terminal's argv to every local API client, so a secret
passed that way would be readable by them. Providers must already be signed in
where herdr-core runs.

**Claude login (`USER`).** On macOS, Claude Code finds its login in the Keychain
using `USER`. Provider `env` lists are applied by the worker itself, so keep
`"env": ["USER"]` in the provider; herdr-core must run as the same user that
signed in. See [coding cells](coding-cells.md#provider-environment).

**Logs.** The `hc` client's output (a mirror of the worker's screen and
`hc`'s own notes) goes to `<run>/herdr/<terminal>.log`, not to the dashboard.
Terminals are named `irr-<run hash>-<cell>-<attempt>` and labelled
`irrigator.run`, `irrigator.cell` and `irrigator.attempt`.

**Watching.**

```sh
hc ls                  # terminals and their labels
hc attach irr-ab12-build-1   # Ctrl+\ detaches without stopping the worker
hc read irr-ab12-build-1     # print recent output
```

Detaching leaves the worker running. Do not type into an attached worker.

## tmux

Each cell's worker process runs in its own window of one tmux session per
run. It works whether or not the controller is itself inside tmux. Check
`tmux -V` (`tmux 3.8`, `tmux 3.3a` and `tmux next-3.6` all parse).

**Minimum version.** tmux 3.2 (`tmuxMinVersion`). An older, missing or
unreadable tmux fails before any worker is launched, naming the found and
required versions.

**Session and windows.** The session is `irrigator-<first 8 of the run id>`,
created detached (`tmux new-session -d`) for the run's first worker; later
workers use `tmux new-window -d`. Windows are named `<cell>-a<attempt>`, with
anything outside `A-Z a-z 0-9 - _` (including `.`, `:` and spaces) replaced by
`_`. `remain-on-exit` is set on each window, never globally, so a finished
worker's screen stays for inspection. Irrigator never edits your tmux config or
touches other sessions. It uses whichever tmux server the controller's
environment selects (for example `$TMUX` or `$TMUX_TMPDIR`), so a controller
running inside tmux creates the session on that server.

**Environment.** A tmux server's environment belongs to the server, not the
controller, so the worker is started through `env` with the same allowlist as
herdr: `PATH`, `NO_COLOR`, `IRRIGATOR_*` and `SWARM_*`, plus worker progress
lines. Every word of the command is shell-quoted. Credentials are never
forwarded (a window's command line is visible in `ps` and to tmux clients), so
providers must already be signed in for the user running the tmux server. See
the `USER` note under herdr.

**Watching.**

```sh
tmux attach -t irrigator-ab12cd34    # attach to the run's session
# prefix n / p / w   next, previous, pick a window (prefix is Ctrl-b by default)
# prefix d           detach; the workers keep running
```

Do not type into a worker's window.

**Exit files and logs.** The worker runs under a small wrapper that writes its
exit code to `<run>/tmux/<window>.exit` (to a temporary name, then renamed). The
controller waits until that file exists. `<run>/tmux/<window>.log` holds only
the waiter's own notes (for example "window is gone"); the worker's output is
not redirected and lives in the window.

**Recovery.** The rules in [Recovery](#recovery) apply, with these tmux
specifics:

- Controller dies, worker keeps running: tmux keeps the window and the worker
  keeps its cell lease; a resumed controller waits for it, as with herdr.
- Live window without a lease: before each launch the controller lists the run
  session's panes and refuses to start a worker for a cell that already has a
  live window (any attempt), or if it cannot read the listing. Wait for it, or
  stop it with `tmux kill-window -t irrigator-XXXXXXXX:<window>`, then resume. A
  dead pane kept by `remain-on-exit` does not count as live.
- The window or pane disappears without an exit file (tmux server killed, window
  closed by hand, worker killed): the outcome is unknown. The controller stops
  with "resume to recover" and the cell is reconciled as usual, never marked
  verified. An exit file left from an earlier launch of the same window name is
  deleted before launching.
- Stopping: the controller signals the pane's process group, falling back to
  `tmux kill-window`.

Real tmux behaviour not covered by tests (which use a fake `tmux`):
`remain-on-exit` timing and pane process groups.

**Cleanup.** Windows and the session stay after the run for inspection. When
done: `tmux kill-session -t irrigator-XXXXXXXX`. Exit files stay in the run
directory.

## Recovery

Executors never change Irrigator's recovery rules (see
[recovery](recovery.md)): interrupted work is never replayed automatically
when it may have had external effects, and a result is verified only from the
worker's own committed output.

- **Controller dies, worker keeps running.** herdr-core keeps the worker, and
  the worker keeps its cell lease. A resumed controller sees the lease held,
  treats the cell as busy and waits; it does not launch a second attempt. If the
  worker then ends without committing a result, the cell is reconciled as usual:
  `uncertain` for providers that are not replay-safe, to be handled with
  `retry --allow-uncertain` (and `--confirm-stopped`).
- **Live terminal without a lease.** Before each launch the controller lists
  `hc ls --json` and refuses to start a worker for a cell that already has a
  running terminal labelled with the same run and cell, whatever its attempt. If
  the listing cannot be read it also refuses. Stop the old terminal with
  `hc kill <name>` or wait for it, then resume.
- **`hc` client exits unexpectedly** (for example herdr-core restarted). The
  controller cannot tell a dead worker from a live one, so it stops with
  "resume to recover". It does not stop a worker it can no longer see. A worker
  that is still alive keeps its lease and is waited for on resume; one that is
  gone is reconciled as above, never marked verified.

Real herdr-core behaviour not covered by tests (which use fake `hc` scripts):
that a terminal and the worker's lock survive client and controller death, and
the exact `hc ls --json` schema. The listing parser accepts a top-level array or
an object with a `terminals` list, labels as an object or `key=value` list, and
treats a terminal with no state as live.

## Adding an executor

An executor is a small named implementation. To add one (for example `tmux`):

1. Add `internal/executor/executor_<name>.go` with a type implementing
   `workerExecutor` (`name()` and `start(...)`, which returns the `*exec.Cmd` the
   controller waits on, a `childProcess` it signals, and a `done` callback) and a
   constructor that checks requirements and fails clearly before any worker runs.
2. Add one entry to `workerExecutors` in `executor_registry.go`.
3. Add it to `doctorExecutors`, and document it here.

The `--executor` flag, the per-run record, precedence and the unknown-name error
come from the registry. The executor must keep the worker holding its own cell
lease and must not forward credentials.

## Cleanup

`irrigator cleanup RUN [--dry-run] [--all-finished]` removes what the herdr and
tmux executors leave behind once a run is done. Nothing in the run directory
changes; the run's durable state, leases and artifacts are untouched.

- **herdr**: lists the run's terminals with
  `hc ls --all --json --label irrigator.run=<run id>` and runs `hc rm` for those
  whose state is `finished` (also `exited`, `dead`, `stopped`). A terminal that is
  running, or reports no state, is kept and listed.
- **tmux**: finds the run's session `irrigator-<first 8 of the run id>`. If every
  pane in it is dead it runs `tmux kill-session`; otherwise it lists the live
  windows and leaves the session alone. A session that no longer exists is
  reported as nothing to do.

An executor whose tool (`hc`, `tmux`) is not installed is skipped. `--dry-run`
lists what would be removed and removes nothing. With `--all-finished`, `RUN` is a
directory of runs and every run directory in it is cleaned; the same
live-is-never-touched rules apply to each. The command prints a JSON report per
run (`removed`, `kept` and a `status` of `cleaned`, `nothing`, `skipped` or
`error` for each executor; under `--dry-run`, `removed` is what would be removed
and the status is `would_clean`) and exits non-zero if any removal or listing failed.
It uses the default tmux server (`TMUX_TMPDIR` selects another), like the
executor itself.
