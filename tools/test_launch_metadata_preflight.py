#!/usr/bin/env python3
"""Workflow-only regression tests; no model, runtime admission or network calls."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

TOOL = Path(__file__).with_name('launch_metadata_preflight.py')

class MetadataCapacityTest(unittest.TestCase):
    def check_plan(self, version='swarm/v6', shape=None, runtime_ok=True):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / 'validator'
            binary.write_text('#!/usr/bin/env python3\nimport sys\nsys.exit(%d)\n' % (0 if runtime_ok else 1))
            binary.chmod(0o700)
            plan = {'api_version': version, 'display': {'version': 1, 'publish': True, 'title': 'Probe', 'purpose': 'Verify explicit capacity'}, 'tasks': [{'id': 'a', 'requires': ['answer'], 'display': {'title': 'Probe', 'why_spawned': 'Check contract', 'done_when': 'Answer verified', 'outcome_field': 'answer'}}]}
            plan.update(shape or {})
            path = root / 'plan.json'
            path.write_text(json.dumps(plan))
            result = subprocess.run([sys.executable, str(TOOL), str(path), '--binary', str(binary)], capture_output=True, text=True, timeout=10)
            return result.returncode, json.loads(result.stdout)

    def test_turn_capacity_all_supported_versions(self):
        for version in range(2, 7):
            with self.subTest(version=version):
                status, result = self.check_plan('swarm/v%d' % version, {'turn_shape': {'root_agents': 1, 'max_turns_per_agent': 1, 'helpers_per_turn': 0}})
                self.assertEqual(status, 0, result)
                self.assertEqual(result['capacity_source'], 'turn_shape')
                self.assertIsNone(result['unknown_capacity_reason'])

    def test_missing_and_malformed_capacity_rejected(self):
        for shape in [None, {'turn_shape': {}}, {'turn_shape': {'root_agents': True, 'max_turns_per_agent': 1, 'helpers_per_turn': 0}}, {'turn_shape': {'root_agents': 1, 'max_turns_per_agent': 1, 'helpers_per_turn': -1}}]:
            self.assertNotEqual(self.check_plan(shape=shape)[0], 0)

    def test_legacy_shape_remains_supported(self):
        status, result = self.check_plan('swarm/v1', {'observer_shape': {'roots': 1, 'levels': 1, 'max_children': 1}})
        self.assertEqual(status, 0)
        self.assertEqual(result['capacity_source'], 'observer_shape')

    def test_runtime_still_authoritative(self):
        shape = {'turn_shape': {'root_agents': 1, 'max_turns_per_agent': 1, 'helpers_per_turn': 0}}
        self.assertNotEqual(self.check_plan(shape=shape, runtime_ok=False)[0], 0)
        self.assertNotEqual(self.check_plan(version='swarm/v1', shape=shape)[0], 0)

if __name__ == '__main__':
    unittest.main()
