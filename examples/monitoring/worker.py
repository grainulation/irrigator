#!/usr/bin/env python3
"""One read-only observation or deterministic judgment over swarm.worker/v1."""
import datetime
import json
import math
import os
import re
import subprocess
import sys

PROTOCOL = "swarm.worker/v1"
IDENTITY = re.compile(r"[A-Za-z0-9_.:/-]{1,256}\Z")
REPOSITORY = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}\Z")
FIELDS = "number,title,state,url,updatedAt"


def target(value):
    if not isinstance(value, dict) or not isinstance(value.get("repository"), str) or not REPOSITORY.fullmatch(value["repository"]):
        raise ValueError("current_facts needs an explicit owner/repository")
    if set(value) == {"repository", "issue"}:
        if type(value["issue"]) is not int or value["issue"] < 1:
            raise ValueError("issue must be a positive integer")
    elif set(value) == {"repository", "search"}:
        query = value["search"]
        if not isinstance(query, str) or not query.strip() or len(query) > 500 or any(ord(c) < 32 for c in query):
            raise ValueError("search must be nonempty text of at most 500 characters")
        # Search is passed as one argument, never interpolated into a shell.
        # Reject repository overrides so the selected repository remains authoritative.
        if re.search(r"\b(?:repo|org|user):", query, re.I):
            raise ValueError("search must not override the selected repository")
    else:
        raise ValueError("choose exactly one issue or search in current_facts")
    return value


def envelope(request):
    if not isinstance(request, dict) or request.get("protocol") != PROTOCOL or request.get("kind") != "task":
        raise ValueError("expected a swarm.worker/v1 task request")
    if not isinstance(request.get("operation_id"), str) or not IDENTITY.fullmatch(request["operation_id"]):
        raise ValueError("invalid operation_id")
    if not isinstance(request.get("task"), dict) or not isinstance(request["task"].get("id"), str) or not IDENTITY.fullmatch(request["task"]["id"]):
        raise ValueError("invalid task identity")
    if type(request.get("attempt")) is not int or request["attempt"] < 1:
        raise ValueError("attempt must be a positive integer")
    if type(request.get("timeout_ms")) is not int or request["timeout_ms"] < 100:
        raise ValueError("timeout_ms must be an integer of at least 100")
    cost = request.get("max_cost_usd")
    if type(cost) not in (int, float) or not math.isfinite(cost) or cost < 0:
        raise ValueError("invalid max_cost_usd")
    context = request.get("context", {})
    if not isinstance(context, dict) or any(not isinstance(k, str) or not isinstance(v, str) for k, v in context.items()):
        raise ValueError("context must contain string values")
    return context


def observation(request, context):
    if set(context) != {"current_facts"}:
        raise ValueError("sensor receives only current_facts")
    facts = target(json.loads(context["current_facts"]))
    repo = facts["repository"]
    if "issue" in facts:
        args = ["gh", "issue", "view", str(facts["issue"]), "--repo", repo, "--json", FIELDS]
    else:
        args = ["gh", "issue", "list", "--repo", repo, "--state", "all", "--search", facts["search"], "--limit", "20", "--json", FIELDS]
    timeout = min(15, max(0.05, request["timeout_ms"] / 1000 - 1))
    result = subprocess.run(args, stdin=subprocess.DEVNULL, capture_output=True, timeout=timeout, check=False)
    if result.returncode:
        # Keep credential-bearing CLI diagnostics out of accepted outputs.
        raise RuntimeError("gh read failed (exit %d); check host authentication and access" % result.returncode)
    if len(result.stdout) > 262144:
        raise ValueError("gh response exceeds 256 KiB")
    data = json.loads(result.stdout)
    rows = [data] if "issue" in facts else data
    if not isinstance(rows, list) or len(rows) > 20 or ("issue" in facts and not isinstance(data, dict)):
        raise ValueError("gh returned an unexpected issue response")
    for row in rows:
        if not isinstance(row, dict) or type(row.get("number")) is not int or row["number"] < 1 or row.get("state") not in ("OPEN", "CLOSED") or any(not isinstance(row.get(k), str) or not row[k] for k in ("title", "url", "updatedAt")):
            raise ValueError("gh returned an incomplete issue record")
    if "issue" in facts and data["number"] != facts["issue"]:
        raise ValueError("gh response does not match the requested issue")
    snapshot = {"source": {"tool": "gh", "host": os.environ.get("GH_HOST", "github.com"), "target": facts,
                           "arguments": args[1:], "request_id": request["request_id"], "operation_id": request["operation_id"]},
                "observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "data": data}
    return {"snapshot": json.dumps(snapshot, separators=(",", ":")),
            "observation_note": "Observation completed; this does not assert the monitored target is complete."}


def judgment(request, context):
    if set(context) != {"proposed_design"}:
        raise ValueError("judge receives only proposed_design")
    proposal = json.loads(context["proposed_design"])
    if not isinstance(proposal, dict) or set(proposal) != {"expected_state"} or proposal["expected_state"] not in ("OPEN", "CLOSED"):
        raise ValueError("proposed_design must name expected_state OPEN or CLOSED")
    dependencies = request.get("dependencies")
    if not isinstance(dependencies, dict) or set(dependencies) != {"sensor"} or not isinstance(dependencies["sensor"], dict) or set(dependencies["sensor"]) != {"snapshot"}:
        raise ValueError("judge requires exactly the selected sensor.snapshot field")
    snapshot = json.loads(dependencies["sensor"]["snapshot"])
    if not isinstance(snapshot, dict) or not isinstance(snapshot.get("source"), dict) or not isinstance(snapshot.get("observed_at"), str) or "data" not in snapshot:
        raise ValueError("snapshot lacks source, timestamp or data")
    target(snapshot["source"].get("target"))
    if datetime.datetime.fromisoformat(snapshot["observed_at"]).tzinfo is None:
        raise ValueError("snapshot timestamp must include its timezone")
    rows = snapshot["data"] if isinstance(snapshot["data"], list) else [snapshot["data"]]
    if any(not isinstance(row, dict) or row.get("state") not in ("OPEN", "CLOSED") or not isinstance(row.get("url"), str) for row in rows):
        raise ValueError("snapshot has invalid evidence")
    matches = [row["state"] == proposal["expected_state"] for row in rows]
    verdict = "inconclusive" if not matches else "pass" if all(matches) else "fail" if not any(matches) else "mixed"
    evidence = {"source": snapshot["source"], "observed_at": snapshot["observed_at"], "expected_state": proposal["expected_state"],
                "issues": [{"url": row["url"], "state": row["state"]} for row in rows],
                "scope": "One observation, at most 20 search results; no claim about later state or unobserved issues."}
    return {"verdict": verdict, "evidence": json.dumps(evidence, separators=(",", ":"))}


def main():
    try:
        raw = sys.stdin.buffer.read(1048577)
        if len(raw) > 1048576:
            raise ValueError("request exceeds 1 MiB")
        request = json.loads(raw)
    except (ValueError, UnicodeError):
        print("Invalid JSON request", file=sys.stderr)
        return 2
    # Without a usable identity there is no response that can safely match a caller.
    identity = request.get("request_id") if isinstance(request, dict) else None
    if not isinstance(identity, str) or not IDENTITY.fullmatch(identity):
        print("Invalid request_id", file=sys.stderr)
        return 2
    response = {"protocol": PROTOCOL, "request_id": identity, "cost_usd": 0, "outcome": "not_started"}
    try:
        context = envelope(request)
        mode = sys.argv[1] if len(sys.argv) == 2 else ""
        if mode == "sensor":
            # Mark the bounded read as dispatched before it can fail or time out.
            response["outcome"] = "settled"
            output = observation(request, context)
        elif mode == "judge":
            output = judgment(request, context)
        else:
            raise ValueError("choose sensor or judge mode")
        response.update(outcome="settled", output=output)
    except (ValueError, TypeError, KeyError, OSError, subprocess.SubprocessError) as error:
        response["error"] = "Observation/judgment failed: " + ("gh read timed out" if isinstance(error, subprocess.TimeoutExpired) else str(error))
    except RuntimeError as error:
        response["error"] = str(error)
    print(json.dumps(response, separators=(",", ":"), allow_nan=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
