#!/usr/bin/env python3
"""Run bounded engine scale checks; local model mode performs real generations.

Example (the binary must include scale benchmark support):
  python3 tools/scale-check.py --binary ./irrigator --dir /tmp/new-scale-check \
      --mode ollama --endpoint http://127.0.0.1:11434 --model MODEL \
      --model-digest SHA256 --counts 900,2000,10000 --workers 6

Models must already be installed on a trusted loopback Ollama server. This tool
never downloads models, starts servers, retries a failed case or calls a paid API.
"""

import argparse
import json
import os
from pathlib import Path
import platform
import resource
import signal
import subprocess
import sys
import time


def parse_counts(raw):
    try:
        values = [int(v) for v in raw.split(",")]
    except ValueError as error:
        raise argparse.ArgumentTypeError("counts must be comma-separated integers") from error
    if not values or len(set(values)) != len(values) or any(v < 1 or v > 10000 for v in values):
        raise argparse.ArgumentTypeError("counts must be distinct values in 1..10000")
    return values


def process_rss(root_pid):
    """Sample a process tree; exited processes between samples are not included."""
    output = subprocess.run(
        ["ps", "-axo", "pid=,ppid=,rss="], capture_output=True, text=True, check=True
    ).stdout
    rows = []
    for line in output.splitlines():
        parts = line.split()
        if len(parts) == 3:
            rows.append(tuple(map(int, parts)))
    selected = {root_pid}
    while True:
        children = {pid for pid, parent, _ in rows if parent in selected}
        if children <= selected:
            break
        selected.update(children)
    return sum(rss * 1024 for pid, _, rss in rows if pid in selected)


def stop_case(process, timeout_seconds, result):
    """Ask the owned controller to pause and drain; never signal saved PIDs."""
    if process.poll() is not None:
        return
    process.send_signal(signal.SIGINT)
    try:
        process.wait(timeout=timeout_seconds + 15)
    except subprocess.TimeoutExpired:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            result["cleanup_uncertain"] = True


def run_case(args, root, count):
    case = root / f"tasks-{count}"
    stdout = root / f"tasks-{count}.stdout.log"
    stderr = root / f"tasks-{count}.stderr.log"
    command = [
        str(args.binary), "benchmark", "--dir", str(case), "--tasks", str(count),
        "--mode", args.mode, "--workers", str(args.workers),
        "--max-output-tokens", str(args.max_output_tokens),
        "--timeout", f"{args.timeout_seconds}s",
    ]
    if args.mode == "ollama":
        command += ["--endpoint", args.endpoint, "--model", args.model, "--model-digest", args.model_digest]
    result = {"tasks": count, "command": command, "passed": False, "samples": 0}
    started = time.monotonic()
    before = resource.getrusage(resource.RUSAGE_CHILDREN)
    with stdout.open("xb") as out, stderr.open("xb") as err:
        process = subprocess.Popen(command, stdout=out, stderr=err, start_new_session=True)
        peak_engine, peak_server = 0, 0
        next_progress = 30
        try:
            while process.poll() is None:
                elapsed = time.monotonic() - started
                too_large = stdout.stat().st_size + stderr.stat().st_size > args.max_log_bytes
                if elapsed > args.wall_timeout or too_large:
                    result["failure"] = "wall_timeout" if not too_large else "log_byte_limit"
                    stop_case(process, args.timeout_seconds, result)
                    break
                try:
                    peak_engine = max(peak_engine, process_rss(process.pid))
                    if args.server_pid:
                        peak_server = max(peak_server, process_rss(args.server_pid))
                    result["samples"] += 1
                except (OSError, subprocess.CalledProcessError, ValueError) as error:
                    errors = result.setdefault("sampling_errors", [])
                    if len(errors) < 20:
                        errors.append(type(error).__name__)
                if elapsed >= next_progress:
                    print(f"tasks={count} mode={args.mode} running elapsed_s={elapsed:.0f} sampled_peak_rss_mb={peak_engine / 1048576:.1f}", flush=True)
                    next_progress += 30
                time.sleep(0.5)
        except KeyboardInterrupt:
            result["failure"] = "operator_interrupt"
            stop_case(process, args.timeout_seconds, result)
        result["exit_code"] = process.wait()
    after = resource.getrusage(resource.RUSAGE_CHILDREN)
    result.update({
        "wall_seconds": time.monotonic() - started,
        "engine_peak_rss_bytes_sampled": peak_engine,
        "server_peak_rss_bytes_sampled": peak_server if args.server_pid else None,
        "waited_children_cpu_seconds": after.ru_utime + after.ru_stime - before.ru_utime - before.ru_stime,
        "waited_children_output_blocks": after.ru_oublock - before.ru_oublock,
        "stdout_bytes": stdout.stat().st_size,
        "stderr_bytes": stderr.stat().st_size,
        "metrics_scope": "RSS is a sampled sum, not an exact peak. Waited-child CPU/I/O includes benchmark descendants plus ps sampling processes; output blocks are OS counters, not physical bytes. Ollama server CPU is not included. Model quality is not measured.",
    })
    report_file = case / "scale-report.json"
    if report_file.exists():
        report = json.loads(report_file.read_text())
        result["engine_report"] = report
        result["passed"] = result["exit_code"] == 0 and report.get("passed") is True and "failure" not in result
    else:
        result.setdefault("failure", "missing_engine_report")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--dir", type=Path, required=True)
    parser.add_argument("--counts", type=parse_counts, default=parse_counts("900,2000,10000"))
    parser.add_argument("--mode", choices=["mock", "ollama"], default="mock")
    parser.add_argument("--workers", type=int, default=6)
    parser.add_argument("--endpoint", default="")
    parser.add_argument("--model", default="")
    parser.add_argument("--model-digest", default="")
    parser.add_argument("--max-output-tokens", type=int, default=32)
    parser.add_argument("--timeout-seconds", type=int, default=30)
    parser.add_argument("--wall-timeout", type=int, default=7200, help="seconds per case")
    parser.add_argument("--max-log-bytes", type=int, default=64 * 1024 * 1024)
    parser.add_argument("--server-pid", type=int, default=0, help="optional owned local server PID to sample separately")
    args = parser.parse_args()
    def interrupt(_signal, _frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGINT, interrupt)
    signal.signal(signal.SIGTERM, interrupt)
    args.binary = args.binary.resolve(strict=True)
    if not os.access(args.binary, os.X_OK):
        parser.error("binary must be executable")
    if not 1 <= args.workers <= 128 or not 1 <= args.max_output_tokens <= 512 or not 1 <= args.timeout_seconds <= 120 or args.wall_timeout < 1 or args.max_log_bytes < 1 or args.server_pid < 0:
        parser.error("invalid concurrency, token, time, log or server-PID bound")
    if args.mode == "ollama" and not all([args.endpoint, args.model, args.model_digest]):
        parser.error("ollama mode requires explicit endpoint, installed model and pinned model digest")
    if args.mode == "mock" and any([args.endpoint, args.model, args.model_digest]):
        parser.error("mock mode cannot claim a local model identity")
    root = args.dir.resolve()
    root.mkdir(mode=0o700)
    report = {"schema": "irrigator.scale-check/v1", "platform": platform.platform(), "mode": args.mode, "planned_tasks": sum(args.counts), "aggregate_output_token_cap": sum(args.counts) * args.max_output_tokens if args.mode == "ollama" else 0, "cases": []}
    for count in args.counts:
        result = run_case(args, root, count)
        report["cases"].append(result)
        report["passed"] = all(case["passed"] for case in report["cases"])
        (root / "scale-check.json").write_text(json.dumps(report, indent=2) + "\n")
        print(f"tasks={count} mode={args.mode} verdict={'PASS' if result['passed'] else 'FAIL'} wall_s={result['wall_seconds']:.2f}", flush=True)
        if not result["passed"]:
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
