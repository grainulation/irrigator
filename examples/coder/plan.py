#!/usr/bin/env python3
"""Print a plan that runs coding agents, each in its own git worktree."""
import argparse
import json
from pathlib import Path
import re
import sys

CELL = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}\Z")
OUTPUTS = ["branch", "commit", "diffstat", "summary"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, help="path of the git repository the agents change")
    parser.add_argument("--base", default="main", help="branch or commit each cell branch starts from")
    parser.add_argument("--model", default="sonnet")
    parser.add_argument("--worktrees", default="", help="worktree root (default: irrigator-worktrees beside the repo)")
    parser.add_argument("--name", default="Worktree coding cells")
    parser.add_argument("--task", action="append", default=[], metavar="ID=PROMPT",
                        help="one coding cell; repeat for more (ID: letters, digits, _ and -)")
    parser.add_argument("--verify", action="append", default=[], metavar="ID=COMMAND",
                        help="shell command run in ID's worktree after the agent finishes; non-zero fails the cell")
    parser.add_argument("--add-dir", action="append", default=[], metavar="DIR",
                        help="extra directory agents may read, outside the worktree (repeatable)")
    parser.add_argument("--continue-from", default="", metavar="RUNID",
                        help="start each cell from irrigator/<RUNID[:8]>/<cell> when that branch exists")
    parser.add_argument("--depends", action="append", default=[], metavar="ID=DEP[,DEP]",
                        help="run ID after DEP and show it DEP's summary")
    args = parser.parse_args()

    tasks = {}
    for spec in args.task:
        cell, sep, prompt = spec.partition("=")
        if not sep or not CELL.match(cell) or not prompt.strip():
            parser.error(f"--task needs ID=PROMPT with a simple ID, got {spec!r}")
        if cell in tasks:
            parser.error(f"duplicate task {cell}")
        tasks[cell] = {"id": cell, "provider": "coder", "prompt": prompt.strip(), "requires": OUTPUTS}
    if not tasks:
        parser.error("give at least one --task ID=PROMPT")
    for spec in args.depends:
        cell, sep, deps = spec.partition("=")
        names = [d for d in deps.split(",") if d]
        if cell not in tasks or not sep or not names or any(d not in tasks or d == cell for d in names):
            parser.error(f"--depends needs ID=DEP[,DEP] naming existing, different tasks, got {spec!r}")
        tasks[cell]["depends_on"] = names
        tasks[cell]["input_fields"] = {d: ["summary"] for d in names}

    context = {}
    for spec in args.verify:
        cell, sep, command = spec.partition("=")
        if cell not in tasks or not sep or not command.strip():
            parser.error(f"--verify needs ID=COMMAND naming an existing task, got {spec!r}")
        key = "verify_" + cell.replace("-", "_")
        if key in context:
            parser.error(f"duplicate --verify for {cell}")
        context[key] = command.strip()
        tasks[cell]["context"] = [key]

    repo = str(Path(args.repo).resolve())
    command = [str(Path(sys.executable).resolve()), str(Path(__file__).with_name("worker.py").resolve()),
               "--repo", repo, "--base", args.base, "--model", args.model]
    if args.worktrees:
        command += ["--worktrees", str(Path(args.worktrees).resolve())]
    for directory in args.add_dir:
        command += ["--add-dir", str(Path(directory).resolve())]
    if args.continue_from:
        command += ["--continue-from", args.continue_from]
    # USER lets Claude Code find its macOS Keychain login; see docs/coding-cells.md.
    plan = {"api_version": "swarm/v1", "name": args.name, "max_tasks": len(tasks),
            "providers": {"coder": {"command": command, "env": ["USER"]}},
            "tasks": list(tasks.values())}
    if context:
        plan["context"] = context
    print(json.dumps(plan, indent=2))


if __name__ == "__main__":
    main()
