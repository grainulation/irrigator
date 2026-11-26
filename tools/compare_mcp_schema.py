#!/usr/bin/env python3
"""Verify description changes without changing an MCP inventory's contract."""

import argparse
import json
import sys


def inventory(capture):
    tools = capture["tools"]
    result = {}
    for tool in tools:
        name = tool["name"]
        if not isinstance(name, str) or not name or name in result:
            raise ValueError("Invalid or duplicate tool name")
        result[name] = tool
    return result


def compare(before, after, expected):
    if not expected:
        raise ValueError("Require at least one expected description change")
    old, new = inventory(before), inventory(after)
    if old.keys() != new.keys():
        raise ValueError("Tool names or counts changed")
    changed = []
    for name in sorted(old):
        old_contract = {k: v for k, v in old[name].items() if k != "description"}
        new_contract = {k: v for k, v in new[name].items() if k != "description"}
        if old_contract != new_contract:
            raise ValueError(f"Non-description contract changed: {name}")
        if old[name].get("description") != new[name].get("description"):
            changed.append(name)
    missing = set(expected) - set(changed)
    if missing:
        raise ValueError("Expected descriptions did not change: " + ", ".join(sorted(missing)))
    return {"tools": len(old), "contract_unchanged": True, "changed_descriptions": changed}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("before")
    parser.add_argument("after")
    parser.add_argument("--changed", action="append", required=True)
    args = parser.parse_args()
    try:
        with open(args.before, encoding="utf-8") as source:
            before = json.load(source)
        with open(args.after, encoding="utf-8") as source:
            after = json.load(source)
        print(json.dumps(compare(before, after, args.changed), indent=2))
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
