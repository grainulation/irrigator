#!/usr/bin/env python3
"""Generate a local one-shot sensor/judge plan with absolute executable paths."""
import argparse
import json
from pathlib import Path
import sys
import runpy

target = runpy.run_path(str(Path(__file__).with_name("worker.py")))["target"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, help="explicit owner/repository")
    selection = parser.add_mutually_exclusive_group(required=True)
    selection.add_argument("--issue", type=int)
    selection.add_argument("--search", help="repository-local issue search, at most 20 results")
    parser.add_argument("--expected-state", choices=("OPEN", "CLOSED"), required=True)
    args = parser.parse_args()
    facts = {"repository": args.repo, "issue" if args.issue is not None else "search": args.issue if args.issue is not None else args.search}
    try:
        target(facts)
    except ValueError as error:
        parser.error(str(error))
    executable = [str(Path(sys.executable).resolve()), str(Path(__file__).with_name("worker.py").resolve())]
    plan = {"api_version": "swarm/v1", "name": "One issue observation and judgment", "max_tasks": 2,
            "context": {"current_facts": json.dumps(facts), "proposed_design": json.dumps({"expected_state": args.expected_state})},
            "providers": {"sensor": {"command": executable + ["sensor"], "env": ["PATH", "HOME", "GH_CONFIG_DIR", "GH_HOST", "GH_TOKEN", "GITHUB_TOKEN", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"]},
                          "judge": {"command": executable + ["judge"], "env": []}},
            "tasks": [{"id": "sensor", "provider": "sensor", "context": ["current_facts"],
                       "prompt": "Read one bounded GitHub issue snapshot. Completion means this read finished, not that the target is complete.",
                       "requires": ["snapshot", "observation_note"]},
                      {"id": "judge", "provider": "judge", "context": ["proposed_design"], "depends_on": ["sensor"],
                       "input_fields": {"sensor": ["snapshot"]}, "prompt": "Compare the observed issue states with proposed_design. Retain evidence and uncertainty.",
                       "requires": ["verdict", "evidence"]}]}
    print(json.dumps(plan, indent=2))


if __name__ == "__main__":
    main()
