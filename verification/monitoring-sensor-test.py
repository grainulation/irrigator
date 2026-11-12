#!/usr/bin/env python3
"""Credential-free protocol and actual CLI integration checks for monitoring."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKER = ROOT / "examples/monitoring/worker.py"
GENERATOR = ROOT / "examples/monitoring/plan.py"
BINARY = Path(os.environ.get("IRRIGATOR_TEST_BINARY", str(ROOT / "irrigator"))).resolve()


class MonitoringTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="irrigator-monitoring-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.receipt = self.root / "gh-args.json"
        fake = self.bin / "gh"
        # This executable replaces gh entirely; these tests cannot reach GitHub.
        fake.write_text("#!" + sys.executable + "\n" + '''import json, os, sys, time
from pathlib import Path
Path(os.environ['TEST_GH_ARGS']).write_text(json.dumps(sys.argv[1:]))
mode = os.environ.get('TEST_GH_MODE', 'ok')
if mode == 'fail': sys.exit(3)
if mode == 'timeout': time.sleep(5)
if mode == 'invalid': print('{}'); sys.exit(0)
row = {'number': 7, 'title': 'Fixture issue', 'state': 'CLOSED', 'url': 'https://example.test/fixture/issues/7', 'updatedAt': '2026-01-01T00:00:00Z'}
if 'list' in sys.argv:
    second = dict(row, number=8, state='OPEN')
    print(json.dumps([] if mode == 'empty' else [row, second]))
else: print(json.dumps(row))
''')
        fake.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + "/usr/bin:/bin", TEST_GH_ARGS=str(self.receipt))
        self.request = {"protocol": "swarm.worker/v1", "kind": "task", "request_id": "request-1", "operation_id": "operation-1",
                        "task": {"id": "sensor"}, "attempt": 1, "timeout_ms": 20000, "max_cost_usd": 0,
                        "context": {"current_facts": json.dumps({"repository": "fixture/project", "issue": 7})}}

    def call(self, request=None, mode="sensor", env=None):
        result = subprocess.run([sys.executable, str(WORKER), mode], input=json.dumps(self.request if request is None else request),
                                text=True, capture_output=True, env=env or self.env, timeout=10, cwd=self.root)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        response = json.loads(result.stdout)
        self.assertEqual(response["protocol"], "swarm.worker/v1")
        self.assertEqual(response["request_id"], (request or self.request)["request_id"])
        return response

    def test_read_and_judge_verdicts(self):
        observed = self.call()
        snapshot = json.loads(observed["output"]["snapshot"])
        self.assertEqual(snapshot["source"]["target"], {"repository": "fixture/project", "issue": 7})
        self.assertTrue(snapshot["observed_at"].endswith("+00:00"))
        self.assertEqual(json.loads(self.receipt.read_text())[:4], ["issue", "view", "7", "--repo"])
        judge = copy.deepcopy(self.request)
        judge.update(context={"proposed_design": '{"expected_state":"CLOSED"}'}, dependencies={"sensor": {"snapshot": observed["output"]["snapshot"]}})
        self.assertEqual(self.call(judge, "judge")["output"]["verdict"], "pass")
        judge["context"]["proposed_design"] = '{"expected_state":"OPEN"}'
        self.assertEqual(self.call(judge, "judge")["output"]["verdict"], "fail")
        self.request["context"]["current_facts"] = '{"repository":"fixture/project","search":"label:ready"}'
        for mode, verdict in (("ok", "mixed"), ("empty", "inconclusive")):
            observed = self.call(env=dict(self.env, TEST_GH_MODE=mode))
            judge["dependencies"]["sensor"]["snapshot"] = observed["output"]["snapshot"]
            self.assertEqual(self.call(judge, "judge")["output"]["verdict"], verdict)
        args = json.loads(self.receipt.read_text())
        self.assertIn("--limit", args)
        self.assertEqual(args[args.index("--limit") + 1], "20")

    def test_known_identity_errors_reply_without_empty_output(self):
        cases = [("kind", "validator"), ("operation_id", ""), ("timeout_ms", True), ("context", {"current_facts": "{"}),
                 ("context", {"current_facts": '{"repository":"--bad/repo","issue":7}'}),
                 ("context", {"current_facts": '{"repository":"fixture/project","search":"repo:elsewhere/project"}'})]
        for key, value in cases:
            with self.subTest(key=key, value=value):
                request = copy.deepcopy(self.request)
                request[key] = value
                response = self.call(request)
                self.assertTrue(response["error"])
                self.assertNotIn("output", response)
        self.assertFalse(self.receipt.exists(), "invalid requests must not launch gh")
        for mode in ("fail", "invalid", "timeout"):
            with self.subTest(gh=mode):
                request = copy.deepcopy(self.request)
                request["timeout_ms"] = 1100
                response = self.call(request, env=dict(self.env, TEST_GH_MODE=mode))
                self.assertTrue(response["error"])
                self.assertNotIn("output", response)

    def test_invalid_json_or_identity_fails_closed(self):
        for raw in ("{", "[]", '{"request_id":null}', '{"request_id":"bad\\nidentity"}'):
            with self.subTest(raw=raw):
                result = subprocess.run([sys.executable, str(WORKER), "sensor"], input=raw, text=True, capture_output=True, env=self.env)
                self.assertEqual(result.returncode, 2)
                self.assertEqual(result.stdout, "")
                self.assertTrue(result.stderr)
        self.assertFalse(self.receipt.exists())

    def test_actual_cli_selected_context_and_dependencies(self):
        self.assertTrue(BINARY.is_file(), "build irrigator first, or set IRRIGATOR_TEST_BINARY")
        generated = subprocess.run([sys.executable, str(GENERATOR), "--repo", "fixture/project", "--issue", "7", "--expected-state", "CLOSED"], check=True, capture_output=True, text=True)
        plan = json.loads(generated.stdout)
        # Fixture-only environment names are explicit just like real provider env grants.
        plan["providers"]["sensor"]["env"] += ["TEST_GH_ARGS", "TEST_GH_MODE"]
        plan_path = self.root / "plan.json"
        plan_path.write_text(json.dumps(plan))
        for mode in ("ok", "fail"):
            run = self.root / ("run-" + mode)
            result = subprocess.run([str(BINARY), "apply", str(plan_path), "--run", str(run), "--backend", "sensor", "--timeout", "20s", "--call-budget", "0.01"],
                                    env=dict(self.env, TEST_GH_MODE=mode), text=True, capture_output=True, timeout=30, cwd=self.root)
            self.assertTrue((run / "state.json").is_file(), result.stderr)
            state = json.loads((run / "state.json").read_text())
            sensor, judge = state["tasks"]["sensor"], state["tasks"]["judge"]
            if mode == "fail":
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(sensor["status"], "failed")
                self.assertEqual(judge["attempt"], 0)
                continue
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((sensor["status"], judge["status"]), ("verified", "verified"))
            self.assertEqual(judge["result"]["verdict"], "pass")
            requests = [json.loads(p.read_text()) for p in run.rglob("*.input.json")]
            received = {r["task"]["id"]: r for r in requests if r.get("kind") == "task"}
            self.assertEqual(set(received["sensor"]["context"]), {"current_facts"})
            self.assertEqual(set(received["judge"]["context"]), {"proposed_design"})
            self.assertEqual(set(received["judge"]["dependencies"]["sensor"]), {"snapshot"})
            self.assertNotIn("observation_note", received["judge"]["dependencies"]["sensor"])


if __name__ == "__main__":
    unittest.main()
