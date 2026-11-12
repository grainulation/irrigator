# Local review

The terminal remains the primary interface. A review can ask for feedback on
code changes, a plan, a direction, or a decision. Explicit review requests open
the selected artifact; review intent is never inferred from every edit.
Snapshots let macOS and Linux users inspect the same material without starting
workers, calling a model, or changing workspace files.

## Plans, direction and decisions

A document review does not require a Git repository. Select a UTF-8 document
inside the declared workspace and create a new snapshot:

```sh
mkdir -p .irrigator/reviews
irrigator review request --kind direction --title 'Choose the next step' --body-file direction.md --workspace "$PWD" --out .irrigator/reviews/direction
irrigator review show .irrigator/reviews/direction
irrigator review comment .irrigator/reviews/direction --file review.md --line 2 --body 'Start with the terminal experience.'
irrigator review feedback .irrigator/reviews/direction
```

`--kind` accepts `plan`, `direction`, or `decision`. A kind describes what is
being reviewed; it does not mean the user approved it. The title, kind and body
are bound to the immutable snapshot identity and retained in feedback. Review
shows these documents as prose rather than labeling them as a code diff. The
selected body is exposed as `review.md` for the shared line-comment workflow.
Line 0 comments on the whole document.

Relative `--body-file` paths resolve from `--workspace`. The file must stay
inside that workspace, must not traverse symlinks and must contain nonempty
UTF-8 text of at most 1 MiB. The command reads only that selected document,
never executes it or scans the rest of the workspace. Keep both the workspace and
output inside the repository or project workspace. Use a new
snapshot for a revised proposal so earlier comments keep their original meaning.

`review show --json` exposes `kind`, `title` and `body` for document requests.
Legacy code snapshots can omit `kind`; consumers should treat omission as
`code`. Document snapshots cannot serve as a code-capture baseline.

## Review queue

`irrigator review` lists everything waiting for you, like a pull-request list
that never leaves your machine: each local branch ahead of the base branch (the
remote default branch, then `main` or `master`; override with `--base`),
uncommitted work, plan/direction/decision snapshots, and files changed on more
than one branch.

```sh
irrigator review                              # the queue
irrigator review change agent-1               # commits, files (risky first), viewed state, comments
irrigator review diff agent-1 --file app.go   # or --commit SHA for one commit
irrigator review viewed agent-1 --file app.go
irrigator review comment --change agent-1 --file app.go --line 12 --body 'Keep validation.'
irrigator review approve agent-1
irrigator review request-changes agent-1 --body 'Split the migration out.'
```

Commits are the record; review state lives in `.irrigator/changes/`, written
only by these commands. A file stays viewed until its content changes. A
verdict binds to the head it was given on: a new commit marks it outdated, and
comments on edited lines show as outdated. Request changes prints feedback as
quoted review data and marks the change as awaiting the agent until a new
commit arrives. Approve a plan or decision with `--snapshot DIR`.

Risk ordering is built in: secrets, authentication, migrations, dependency
manifests and build/CI files come first; lockfiles and generated files last.
It orders attention and approves nothing.

### Interactive review

```sh
irrigator review tui [--workspace DIR] [--base REF]
```

A terminal UI (Bubble Tea) over the same data as `review list` and
`review diff`. The left pane lists review branches with status and a marker
for files changed on more than one branch; the right pane shows the selected
branch's colored, scrollable diff. It needs a terminal on stdin and stdout and
refuses otherwise. It uses the alternate screen and restores the terminal on
exit, panic and signals. `NO_COLOR` disables color.

| Key | Action |
| --- | --- |
| `j`/`k`, arrows | move or scroll |
| `Enter`, `Tab` | switch pane |
| `n` / `p` | next / previous file or hunk |
| `a` | approve |
| `r` | request changes (prompts for a one-line note) |
| `y` | copy the selected branch name (OSC 52) |
| `Y` | copy `git diff <base>...<branch>` (OSC 52) |
| `?` | help |
| `q` | quit |

Decisions go through the same code as `review approve` and
`review request-changes`; the TUI has no store of its own.

### Cleaning up

`irrigator review` shows how much space local Irrigator data uses.
`irrigator review prune` previews what can go: review snapshots and agent
session logs older than 30 days (`--older-than DAYS`), and review state for
deleted branches or removed snapshots. Add `--apply` to delete them. Commits and branches are never touched.

## Code changes

Run these commands from a Git repository root. Without `--out`, snapshots are
saved under `.irrigator/reviews/`, which review capture excludes from its own
input; the command prints the path. Name a snapshot there when you want to refer
to it later:

```sh
irrigator review capture --workspace "$PWD" --out .irrigator/reviews/start
# Work in the repository, then compare against its exact earlier contents:
irrigator review capture --workspace "$PWD" --baseline .irrigator/reviews/start --out .irrigator/reviews/changes
irrigator review show .irrigator/reviews/changes
irrigator review comment .irrigator/reviews/changes --file src/main.go --line 12 --body 'Preserve the existing validation.'
irrigator review feedback .irrigator/reviews/changes --out .irrigator/reviews/feedback.txt
```

The first capture compares the current workspace against HEAD. An explicit
`--baseline` compares against the earlier snapshot's captured workspace instead,
including edits and untracked files already present then. Nothing is attributed
to a worker merely because it changed during that interval. The capture requires
the Git repository root; it does not silently include a parent repository.

`review show SNAPSHOT --json` exposes file paths, statuses, unified diffs, before
and after hashes/content, capture time, base commit, baseline identity and
comments. A private inventory records a hash of every reviewed file and keeps
the contents of files that differ from HEAD; unchanged files are referenced by
their Git object, so snapshot size follows the size of the change rather than
the repository. A baseline comparison reads those objects from the repository. These snapshots contain source code; keep them out of
public commits and share them deliberately.

Snapshots are immutable and content-addressed. Later edits do not change an open
review. Comments go into a separate locked journal and reference the snapshot
identity. Line numbers address the captured new file, or the old file for a
deletion. Line 0 attaches a comment to the file as a whole. Binary files support
file comments and show a summary rather than interpreting their contents.
Feedback export quotes comments as review data and grants no authority to run
commands, spend money or apply changes. Sending it to a coordinator remains an
explicit separate step. Review comments, passing checks, human approval and
applying changes are distinct facts; none implies the others.

Capture includes tracked files plus nonignored untracked files, including staged
and unstaged edits. It reads actual bytes so Git's assume-unchanged flag cannot
hide a modification. It disables external diff drivers, text conversion,
filesystem monitors and hooks for its Git commands. Symlinks are recorded as
their target text and never followed. Submodules and nested repositories are
skipped. Unsafe path names and special files are rejected. Files over 2 MiB are
compared by hash and reported as changed without their contents. Changed
contents are limited to 24 MiB per snapshot and workspaces to 100000 files.
If files or HEAD change while capture runs, capture fails and can be retried.
The capture does not provide a filesystem transaction or prevent other programs
from modifying the workspace.

An explicit `--out` must be a new directory whose parent exists; under
`.irrigator/reviews/` the parent is created. Recapturing identical content
without `--out` reuses the saved snapshot. `review feedback --out` refuses
to overwrite an existing file. With no `--out`, feedback prints to stdout.

Capture and request print a concise summary by default. Add `--json` for the
full structured snapshot for scripts and hosts.
