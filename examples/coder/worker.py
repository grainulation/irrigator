#!/usr/bin/env python3
"""swarm.worker/v1 command worker that runs one Claude Code agent in a git worktree.

Each cell gets branch irrigator/<run8>/<cell> in its own worktree (one per cell
and attempt), and one `claude -p` run there, bounded by the request's
max_cost_usd (via --max-budget-usd) and timeout. Results land as branches for
`irrigator review list`.

Outcomes: not_started when nothing was dispatched (wrong request kind, git
setup failed, claude could not be launched); uncertain when the agent timed out,
ended without a result, or git failed after it ran; settled otherwise.

Verification: if the request context has verify_<cell id, '-' as '_'>, that shell
command runs in the worktree after an error-free finish with commits; non-zero
exit settles with an error carrying the output tail. A usage-limit result is
reported as such, with its reset time.

A retry continues the cell's branch: earlier clean worktrees for it are removed
(a dirty one makes the retry not_started) and the agent is told to build on the
commits already there. See cleanup.py to remove a run's worktrees and branches.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import threading

PROTOCOL = "swarm.worker/v1"
ALLOWED = ["Read", "Edit", "Write", "Glob", "Grep", "TodoWrite",
           "Bash(go:*)", "Bash(gofmt:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git show:*)",
           "Bash(git log:*)", "Bash(git add:*)", "Bash(git commit:*)", "Bash(git mv:*)", "Bash(make:*)",
           "Bash(python3:*)", "Bash(node:*)", "Bash(npm:*)", "Bash(npx:*)",
           "Bash(ls:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(grep:*)",
           "Bash(find:*)", "Bash(diff:*)", "Bash(mkdir:*)", "Bash(cp:*)", "Bash(mv:*)", "Bash(chmod:*)"]
DENIED = ["Bash(git push:*)", "Bash(git reset:*)", "Bash(git checkout:*)", "Bash(git switch:*)",
          "Bash(rm:*)", "WebFetch", "WebSearch"]


class Progress:
    """Progress lines go to stderr and, when there is a terminal, to /dev/tty."""

    def __init__(self, enabled=True):
        self.tty = None
        if enabled:
            try:
                self.tty = open("/dev/tty", "w", buffering=1)
            except OSError:
                pass

    def say(self, cell, text):
        line = f"[{cell}] {text}"
        print(line, file=sys.stderr, flush=True)
        if self.tty:
            try:
                self.tty.write(line[:220] + "\n")
            except OSError:
                self.tty = None


def git(repo, *args):
    r = subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True, stdin=subprocess.DEVNULL)
    if r.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)}: {r.stderr.strip()}")
    return r.stdout.strip()


def branch_worktrees(repo, branch):
    """Paths of worktrees that have `branch` checked out."""
    paths, current = [], None
    for line in git(repo, "worktree", "list", "--porcelain").splitlines():
        if line.startswith("worktree "):
            current = line[len("worktree "):]
        elif line == f"branch refs/heads/{branch}" and current:
            paths.append(current)
    return paths


class DirtyWorktree(Exception):
    pass


def retire_earlier_attempts(repo, branch):
    """Remove clean worktrees holding the cell's branch; refuse ones with uncommitted work."""
    git(repo, "worktree", "prune")
    for path in branch_worktrees(repo, branch):
        if os.path.realpath(path) == os.path.realpath(repo):
            raise DirtyWorktree(f"branch {branch} is checked out in the main repository at {path}")
        if git(path, "status", "--porcelain"):
            raise DirtyWorktree(f"an earlier attempt's worktree at {path} has uncommitted work; "
                                f"inspect it, commit or discard the work, then retry")
        git(repo, "worktree", "remove", path)


LIMIT = re.compile(r"\b(session|usage|rate) limit\b|\blimit reached\b", re.I)
RESET = re.compile(r"(\bresets?\b.*|\btry again\b.*)", re.I)


def limit_error(text):
    """A plain message when the agent's result says the account hit a usage limit, else ''."""
    line = next((ln.strip() for ln in text.splitlines() if LIMIT.search(ln)), "")
    if not line:
        return ""
    found = RESET.search(line) or RESET.search(text)
    when = found.group(1).strip()[:200] if found else "reset time not stated"
    return (f"account usage limit reached, not a code problem: {line[:200]} (reset: {when}). "
            "Resume this cell after the limit resets.")


def format_usage(raw_usage, model=None):
    if not isinstance(raw_usage, dict):
        return None
    def _int(k):
        v = raw_usage.get(k)
        if isinstance(v, (int, float)) and v >= 0:
            return int(v)
        return 0
    inp = _int("input_tokens")
    out = _int("output_tokens")
    thinking = _int("thinking_tokens")
    cache_read = _int("cache_read_input_tokens") or _int("cache_read_tokens")
    cache_write = _int("cache_creation_input_tokens") or _int("cache_write_tokens")
    total = _int("total_tokens") or (inp + out + thinking + cache_read + cache_write)
    u = {
        "input_tokens": inp,
        "output_tokens": out,
        "thinking_tokens": thinking,
        "cache_read_tokens": cache_read,
        "cache_write_tokens": cache_write,
        "total_tokens": total,
    }
    m = model or raw_usage.get("model")
    if m and isinstance(m, str):
        u["model"] = m
    return u


def respond(req, outcome, output=None, error="", cost=None, usage=None, model=None):
    msg = {"protocol": PROTOCOL, "request_id": req.get("request_id", ""), "outcome": outcome}
    if output:
        msg["output"] = output
    if error:
        msg["error"] = error
    if cost is not None:
        msg["cost_usd"] = round(float(cost), 6)
    if usage:
        msg["usage"] = usage
    if model:
        msg["model"] = model
    sys.stdout.write(json.dumps(msg))
    sys.stdout.flush()


def prompt_for(req, worktree, branch, base, previous=""):
    parts = [req["task"].get("prompt", "")]
    context = dict(req.get("context") or {})
    if previous:
        context["previous_attempts"] = previous
    for name, value in sorted(context.items()):
        parts.append(f"\n## Context: {name}\n{value}")
    for dep, fields in sorted((req.get("dependencies") or {}).items()):
        parts.append(f"\n## Output of dependency {dep}\n" + json.dumps(fields, indent=2))
    parts.append(f"""
## Working rules
- You are in a git worktree at {worktree} on branch {branch}, created from {base}.
  Only change files inside it. Do not push, reset, or switch branches.
- Follow the repository's AGENTS.md and CONTRIBUTING.md if present.
- Keep the change focused on this task. Add or update tests for behavior you change.
- Commit your work with a clear message.
- Finish with a short summary: what changed, which tests ran and their result, and anything left undone.""")
    return "\n".join(parts)


def describe(block, worktree):
    if block.get("type") == "tool_use":
        inp = block.get("input", {})
        detail = inp.get("file_path") or inp.get("command") or inp.get("pattern") or ""
        return f"▸ {block.get('name')} {str(detail).replace(os.path.join(worktree, ''), '')}"
    if block.get("type") == "text" and block.get("text", "").strip():
        return "· " + block["text"].strip().splitlines()[0]
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--repo", required=True, help="path of the git repository")
    ap.add_argument("--base", default="main", help="branch or commit new cell branches start from")
    ap.add_argument("--model", default="sonnet")
    ap.add_argument("--worktrees", default="", help="worktree root (default: irrigator-worktrees beside the repo)")
    ap.add_argument("--add-dir", action="append", default=[], metavar="DIR",
                    help="extra directory the agent may read, outside the worktree (repeatable)")
    ap.add_argument("--continue-from", default="", metavar="RUNID",
                    help="earlier run id; a new cell branch starts from its irrigator/<RUNID[:8]>/<cell> branch")
    ap.add_argument("--no-tty", action="store_true", help="do not write progress to /dev/tty")
    args = ap.parse_args()
    req = json.loads(sys.stdin.read())
    cell = (req.get("task") or {}).get("id", "cell")
    if req.get("kind") != "task":
        return respond(req, "not_started", error="worktree coder only runs tasks, not validators")

    progress = Progress(not args.no_tty)
    run8 = (req.get("run_id") or "run")[:8]
    branch = f"irrigator/{run8}/{cell}"
    root = args.worktrees or os.path.join(os.path.dirname(os.path.abspath(args.repo)), "irrigator-worktrees")
    worktree = os.path.join(root, run8, f"{cell}-a{req.get('attempt', 1)}")
    previous = ""
    try:
        base_sha = git(args.repo, "rev-parse", "--verify", args.base + "^{commit}")
        if git(args.repo, "branch", "--list", branch):
            retire_earlier_attempts(args.repo, branch)
            git(args.repo, "worktree", "add", worktree, branch)  # a retry continues the cell's branch
            ahead = git(args.repo, "rev-list", "--count", f"{base_sha}..{branch}")
            if ahead != "0":
                log = git(args.repo, "log", "--format=%h %s", f"{base_sha}..{branch}")
                previous = (f"Branch {branch} already has {ahead} commit(s) beyond {args.base} from an earlier "
                            f"attempt. Review that work (git log, git diff {args.base}...HEAD) and continue it "
                            f"rather than starting over.\n\nCommits so far:\n{log}")
        else:
            start = base_sha
            earlier = f"irrigator/{args.continue_from[:8]}/{cell}" if args.continue_from else ""
            if earlier and earlier != branch and git(args.repo, "branch", "--list", earlier):
                start = git(args.repo, "rev-parse", earlier)  # build on an earlier run's work
            git(args.repo, "worktree", "add", "-b", branch, worktree, start)
            ahead = git(args.repo, "rev-list", "--count", f"{base_sha}..{branch}")
            if ahead != "0":
                log = git(args.repo, "log", "--format=%h %s", f"{base_sha}..{branch}")
                previous = (f"Branch {branch} was started from {earlier} and already has {ahead} commit(s) "
                            f"beyond {args.base}. Review that work (git log, git diff {args.base}...HEAD) "
                            f"and continue it rather than starting over.\n\nCommits so far:\n{log}")
    except DirtyWorktree as e:
        return respond(req, "not_started", error=f"retry refused: {e}")
    except Exception as e:  # nothing dispatched yet
        return respond(req, "not_started", error=f"worktree setup failed: {e}")

    budget = max(0.01, float(req.get("max_cost_usd") or 1))
    seconds = max(1.0, (req.get("timeout_ms") or 1800000) / 1000 - 30)
    cmd = ["claude", "-p", prompt_for(req, worktree, branch, args.base, previous), "--model", args.model,
           "--output-format", "stream-json", "--verbose", "--max-budget-usd", f"{budget:.2f}",
           "--permission-mode", "acceptEdits", "--setting-sources", "", *[x for d in args.add_dir for x in ("--add-dir", d)],
           "--allowedTools", ",".join(ALLOWED), "--disallowedTools", ",".join(DENIED)]
    progress.say(cell, f"start · {branch} · model {args.model} · budget ${budget:.2f}")
    try:
        proc = subprocess.Popen(cmd, cwd=worktree, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, text=True)
    except OSError as e:  # the agent never launched
        return respond(req, "not_started", error=f"could not launch claude: {e}")

    timed_out = threading.Event()

    def expire():
        timed_out.set()
        proc.kill()

    timer = threading.Timer(seconds, expire)
    timer.start()
    result = None
    try:
        for line in proc.stdout:
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            if ev.get("type") == "assistant":
                for block in ev.get("message", {}).get("content", []):
                    text = describe(block, worktree)
                    if text:
                        progress.say(cell, text)
            elif ev.get("type") == "result":
                result = ev
    finally:
        timer.cancel()
        proc.wait()
    cost = (result or {}).get("total_cost_usd")
    raw_usage = (result or {}).get("usage")
    model = (result or {}).get("model") or args.model
    usage = format_usage(raw_usage, model)

    if timed_out.is_set() or result is None:
        # The agent ran and may have changed files; never claim a clean outcome.
        progress.say(cell, "timed out or ended without a result")
        return respond(req, "uncertain", error="agent timed out or ended without a result", cost=cost, usage=usage, model=model)

    try:
        if git(worktree, "status", "--porcelain"):
            git(worktree, "add", "-A")
            git(worktree, "commit", "-q", "-m", f"irrigator cell {cell}: uncommitted agent changes")
            progress.say(cell, "committed leftover changes")
        commits = int(git(worktree, "rev-list", "--count", f"{base_sha}..HEAD") or 0)
        output = {
            "branch": branch,
            "commit": git(worktree, "rev-parse", "HEAD"),
            "diffstat": git(worktree, "diff", "--shortstat", f"{base_sha}...HEAD") or "no changes",
            "summary": (result.get("result") or "").strip()[:4000] or "(no summary)",
        }
    except Exception as e:  # the agent ran, so its effects are unconfirmed
        return respond(req, "uncertain", error=f"agent finished but git failed: {e}", cost=cost, usage=usage, model=model)
    if result.get("is_error"):
        progress.say(cell, f"failed · ${cost or 0:.2f}")
        text = (result.get("result") or result.get("subtype") or "unknown").strip()
        limited = limit_error(text)
        if limited:
            return respond(req, "settled", error=limited, cost=cost, usage=usage, model=model)
        return respond(req, "settled", error=f"agent error: {text.splitlines()[0][:300]}", cost=cost, usage=usage, model=model)
    if commits == 0:
        progress.say(cell, f"no changes · ${cost or 0:.2f}")
        return respond(req, "settled", error="agent finished without changing anything", cost=cost, usage=usage, model=model)
    verify = ((req.get("context") or {}).get("verify_" + cell.replace("-", "_")) or "").strip()
    if verify:
        progress.say(cell, f"verify: {verify}")
        try:
            v = subprocess.run(["/bin/sh", "-c", verify], cwd=worktree, capture_output=True, text=True,
                               stdin=subprocess.DEVNULL, timeout=max(60.0, seconds))
        except subprocess.TimeoutExpired:
            return respond(req, "uncertain", error=f"verification `{verify}` timed out", cost=cost, usage=usage, model=model)
        if v.returncode != 0:
            progress.say(cell, f"verify FAILED (exit {v.returncode})")
            tail = (v.stdout + v.stderr).strip()[-1500:]
            return respond(req, "settled", error=f"verification failed (exit {v.returncode}): {tail}", cost=cost, usage=usage, model=model)
        progress.say(cell, "verify passed")
        output["summary"] += f"\n\nVerification `{verify}` passed."
    wanted = req["task"].get("requires") or list(output)
    output = {k: output.get(k, output["summary"]) for k in wanted}
    progress.say(cell, f"done · {commits} commit(s) · {output.get('diffstat', '')} · ${cost or 0:.2f}")
    respond(req, "settled", output=output, cost=cost, usage=usage, model=model)


if __name__ == "__main__":
    main()
