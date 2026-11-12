#!/usr/bin/env python3
"""Opt-in Claude lifecycle capture. Metadata only; never writes Irrigator runs."""
import argparse
import datetime
import json
import os
from pathlib import Path
import re
import shlex
import stat
import sys
import uuid

LIMIT = 65536
EVENTS = {"SubagentStart": "active", "SubagentStop": "stopped"}


def directory(path):
    """Pin each directory component; reject symlinks, even in ancestors."""
    path = os.path.abspath(path)
    fd = os.open(os.path.sep, os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in Path(path).parts[1:]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        info = os.fstat(fd)
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
            raise ValueError("event directory must be owned by this user with mode 0700")
        return fd
    except BaseException:
        os.close(fd)
        raise


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def identifier(value):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_.:@-]{1,160}", value):
        raise ValueError("invalid host identity")
    return value


def normalize(raw):
    if len(raw) > LIMIT:
        raise ValueError("host event exceeds limit")
    value = json.loads(raw, object_pairs_hook=unique_pairs)
    if not isinstance(value, dict) or value.get("hook_event_name") not in EVENTS:
        raise ValueError("unsupported host event")
    # Prompts, response text, cwd and transcript paths are deliberately discarded.
    return {"schema": "irrigator.host-agent/v1", "host": "claude-code",
            "session_id": identifier(value.get("session_id")),
            "agent_id": identifier(value.get("agent_id")),
            "agent_type": identifier(value.get("agent_type")),
            "event": value["hook_event_name"], "status": EVENTS[value["hook_event_name"]],
            "received_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "evidence": "host_reported", "runtime_task": False}


def record(path, raw):
    value = normalize(raw)
    root = directory(path)
    try:
        name = uuid.uuid4().hex + ".json"
        pending = "." + name
        fd = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=root)
        with os.fdopen(fd, "w") as f:
            json.dump(value, f, sort_keys=True)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.rename(pending, name, src_dir_fd=root, dst_dir_fd=root)
    finally:
        os.close(root)


def snapshot(path):
    root = directory(path)
    rows = {}
    try:
        names = os.listdir(root)
        if len(names) > 10000:
            raise ValueError("event directory exceeds 10000 entries; rotate capture directory")
        for name in names:
            if not re.fullmatch(r"[0-9a-f]{32}\.json", name):
                continue
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
            with os.fdopen(fd, "rb") as f:
                if not stat.S_ISREG(os.fstat(f.fileno()).st_mode):
                    raise ValueError("event must be a regular file")
                raw = f.read(LIMIT + 1)
            if len(raw) > LIMIT:
                raise ValueError("stored event exceeds limit")
            item = json.loads(raw, object_pairs_hook=unique_pairs)
            if item.get("schema") != "irrigator.host-agent/v1" or item.get("event") not in EVENTS:
                raise ValueError("invalid stored event")
            timestamp = item.get("received_at")
            if not isinstance(timestamp, str) or len(timestamp) > 64:
                raise ValueError("invalid event timestamp")
            parsed = datetime.datetime.fromisoformat(timestamp)
            if parsed.tzinfo is None:
                raise ValueError("event timestamp requires timezone")
            # Stored files are untrusted input too: reconstruct the allowlist and
            # derive certainty/status; never return arbitrary stored fields.
            item = normalize(json.dumps({"hook_event_name": item["event"],
                                         "session_id": item.get("session_id"),
                                         "agent_id": item.get("agent_id"),
                                         "agent_type": item.get("agent_type")}))
            item["received_at"] = parsed.astimezone(datetime.timezone.utc).isoformat()
            key = (identifier(item["session_id"]), identifier(item["agent_id"]))
            if key not in rows or item["received_at"] > rows[key]["received_at"]:
                rows[key] = item
        return {"schema": "irrigator.host-agents/v1", "agents": list(rows.values()),
                "note": "Host-reported lifecycle only. Stopped is not verified; active may be stale after a missing stop event."}
    finally:
        os.close(root)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["record", "status", "settings"])
    parser.add_argument("--directory", required=True, help="existing private mode-0700 capture directory")
    args = parser.parse_args()
    if args.command == "record":
        record(args.directory, sys.stdin.buffer.read(LIMIT + 1))
    elif args.command == "status":
        print(json.dumps(snapshot(args.directory), indent=2))
    else:
        fd = directory(args.directory)
        os.close(fd)
        cmd = shlex.join([sys.executable, "-I", str(Path(__file__).resolve()), "record", "--directory", os.path.abspath(args.directory)])
        print(json.dumps({"hooks": {event: [{"hooks": [{"type": "command", "command": cmd, "timeout": 5}]}] for event in EVENTS}}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, RecursionError, KeyError, TypeError):
        print("Host event capture rejected invalid input or inaccessible private storage.", file=sys.stderr)
        sys.exit(1)
