#!/usr/bin/env python3
"""Capture a local MCP tool inventory without calling tools or starting runs."""

import argparse
import json
import os
import pathlib
import select
import subprocess
import sys
import time


MAX_RESPONSE_BYTES = 4 * 1024 * 1024


def read_mcp_line(stream, pending, deadline):
    """Consume one bounded line without extending the deadline after a byte."""
    while True:
        newline = pending.find(b"\n")
        if newline >= 0:
            if newline > MAX_RESPONSE_BYTES:
                raise RuntimeError("MCP response exceeded capture bound")
            line = bytes(pending[:newline + 1])
            del pending[:newline + 1]
            return line
        if len(pending) > MAX_RESPONSE_BYTES:
            raise RuntimeError("MCP response exceeded capture bound")
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([stream], [], [], remaining)[0]:
            raise RuntimeError("MCP response timed out")
        chunk = os.read(stream.fileno(), min(65536, MAX_RESPONSE_BYTES + 1 - len(pending)))
        if not chunk:
            raise RuntimeError("MCP response ended before a complete line")
        pending.extend(chunk)


MAX_DISCOVERY_PAGES = 100
MAX_DISCOVERY_TOOLS = 100


def discover_tools(exchange):
    tools, seen_names, seen_cursors = [], set(), set()
    cursor = None
    for page_index in range(MAX_DISCOVERY_PAGES):
        page = exchange(2 + page_index, "tools/list", {"cursor": cursor} if cursor is not None else {})
        rows = page.get("tools")
        if not isinstance(rows, list):
            raise RuntimeError("Invalid tools discovery page")
        for tool in rows:
            if not isinstance(tool, dict) or not isinstance(tool.get("name"), str) or not tool["name"] or tool["name"] in seen_names:
                raise RuntimeError("Invalid or duplicate tool name")
            seen_names.add(tool["name"])
        tools.extend(rows)
        if len(tools) > MAX_DISCOVERY_TOOLS:
            raise RuntimeError("Excessive discovery tool count")
        following = page.get("nextCursor")
        if following is None:
            return tools
        if not isinstance(following, str) or not following or len(following) > 4096 or following in seen_cursors:
            raise RuntimeError("Invalid or repeated discovery cursor")
        seen_cursors.add(following)
        cursor = following
    raise RuntimeError("Excessive discovery page count")


def write_mcp_request(stream, data, deadline):
    """Do not let an unresponsive reader block discovery before its timeout."""
    fd = stream.fileno()
    os.set_blocking(fd, False)
    offset = 0
    while offset < len(data):
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not select.select([], [stream], [], remaining)[1]:
            raise RuntimeError("MCP request timed out")
        try:
            size = os.write(fd, data[offset:offset + 4096])
        except BlockingIOError:
            continue
        if size <= 0:
            raise RuntimeError("MCP request pipe closed")
        offset += size


def capture(binary, allow_control=False, canonical_names=False):
    arguments = [str(pathlib.Path(binary).resolve()), "mcp"]
    if allow_control:
        arguments.append("--allow-control")
    if canonical_names:
        arguments.append("--canonical-names")
    process = subprocess.Popen(
        arguments,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        bufsize=0,
    )
    try:
        pending = bytearray()
        capture_deadline = time.monotonic() + 30

        def exchange(identifier, method, params):
            request = {"jsonrpc": "2.0", "id": identifier, "method": method, "params": params}
            deadline = min(time.monotonic() + 15, capture_deadline)
            write_mcp_request(process.stdin, json.dumps(request).encode() + b"\n", deadline)
            while True:
                line = read_mcp_line(process.stdout, pending, deadline)
                response = json.loads(line)
                if response.get("id") == identifier:
                    if "error" in response:
                        raise RuntimeError(f"MCP {method} failed: {response['error']}")
                    return response["result"]

        initialized = exchange(1, "initialize", {
            "protocolVersion": "2025-06-18", "capabilities": {},
            "clientInfo": {"name": "irrigator-schema-capture", "version": "1"},
        })
        write_mcp_request(process.stdin, b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n', capture_deadline)
        tools = discover_tools(exchange)
        return {
            "serverName": initialized["serverInfo"]["name"],
            "serverInfo": initialized["serverInfo"],
            "protocolVersion": initialized["protocolVersion"],
            "startupArguments": arguments[1:],
            "tools": tools,
        }
    finally:
        process.terminate()
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=2)
        process.stdin.close()
        process.stdout.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="./irrigator")
    parser.add_argument("--allow-control", action="store_true")
    parser.add_argument("--canonical-names", action="store_true")
    args = parser.parse_args()
    try:
        print(json.dumps(capture(args.binary, args.allow_control, args.canonical_names), indent=2))
    except (OSError, ValueError, KeyError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
