#!/usr/bin/env python3
"""List and remove the worktrees (and optionally branches) of one coder run.

Without --force, worktrees with uncommitted work are refused and their branches
kept. Use --dry-run to only list.
"""
import argparse
import os
import subprocess
import sys


def git(repo, *args):
    r = subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True, stdin=subprocess.DEVNULL)
    if r.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)}: {r.stderr.strip()}")
    return r.stdout.strip()


def run_worktrees(repo, run8):
    """(path, branch) of worktrees on branches irrigator/<run8>/*."""
    found, path = [], None
    for line in git(repo, "worktree", "list", "--porcelain").splitlines():
        if line.startswith("worktree "):
            path = line[len("worktree "):]
        elif line.startswith(f"branch refs/heads/irrigator/{run8}/"):
            found.append((path, line[len("branch refs/heads/"):]))
    return found


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--repo", required=True)
    ap.add_argument("--run", required=True, help="run ID (the first eight characters are used)")
    ap.add_argument("--delete-branches", action="store_true", help="also delete the run's branches (git branch -D)")
    ap.add_argument("--force", action="store_true", help="remove dirty worktrees too, discarding uncommitted work")
    ap.add_argument("--dry-run", action="store_true", help="list what would be removed and change nothing")
    args = ap.parse_args()
    run8 = args.run[:8]
    if not run8:
        print("empty --run", file=sys.stderr)
        return 2
    repo = args.repo
    git(repo, "worktree", "prune")
    refused = set()
    for path, branch in run_worktrees(repo, run8):
        if os.path.realpath(path) == os.path.realpath(repo):
            print(f"skip   {path} (main repository) {branch}")
            refused.add(branch)
            continue
        dirty = bool(git(path, "status", "--porcelain"))
        note = " (dirty)" if dirty else ""
        if dirty and not args.force:
            print(f"refuse {path} {branch}: uncommitted work (use --force to discard it)")
            refused.add(branch)
        elif args.dry_run:
            print(f"would remove {path} {branch}{note}")
        else:
            git(repo, "worktree", "remove", *(["--force"] if dirty else []), path)
            print(f"removed {path} {branch}{note}")
    branches = git(repo, "branch", "--list", f"irrigator/{run8}/*", "--format=%(refname:short)").splitlines()
    for branch in filter(None, branches):
        if not args.delete_branches:
            print(f"kept   branch {branch}")
        elif branch in refused:
            print(f"kept   branch {branch} (its worktree was not removed)")
        elif args.dry_run:
            print(f"would delete branch {branch}")
        else:
            git(repo, "branch", "-D", branch)
            print(f"deleted branch {branch}")
    return 1 if refused else 0


if __name__ == "__main__":
    sys.exit(main())
