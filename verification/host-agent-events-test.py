#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('capture', Path(__file__).resolve().parents[1] / 'tools/host-agent-events.py')
capture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capture)

class HostEvents(unittest.TestCase):
    def test_lifecycle_redaction(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = str(Path(tmp).resolve())
            event = dict(session_id='session-1', agent_id='a', agent_type='Explore', hook_event_name='SubagentStart', last_assistant_message='SECRET', transcript_path='/secret', prompt='SECRET')
            capture.record(root, json.dumps(event))
            self.assertEqual(capture.snapshot(root)['agents'][0]['status'], 'active')
            event['hook_event_name'] = 'SubagentStop'
            capture.record(root, json.dumps(event))
            result = capture.snapshot(root)
            self.assertEqual(result['agents'][0]['status'], 'stopped')
            self.assertFalse(result['agents'][0]['runtime_task'])
            self.assertNotIn('SECRET', json.dumps(result))
            for path in Path(root).iterdir():
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
    def test_stored_claims_cannot_escalate_or_leak(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = str(Path(tmp).resolve())
            capture.record(root, json.dumps(dict(session_id='s', agent_id='a', agent_type='Explore', hook_event_name='SubagentStop')))
            path = next(Path(root).glob('*.json'))
            item = json.loads(path.read_text())
            item.update(status='verified', runtime_task=True, evidence='production', prompt='SECRET')
            path.write_text(json.dumps(item))
            result = capture.snapshot(root)['agents'][0]
            self.assertEqual(result['status'], 'stopped')
            self.assertEqual(result['evidence'], 'host_reported')
            self.assertFalse(result['runtime_task'])
            self.assertNotIn('SECRET', json.dumps(result))

    def test_rejections(self):
        for raw in ['{}', 'null', '{"session_id":"a","session_id":"b"}', 'x'*65537]:
            with self.assertRaises((ValueError, TypeError)):
                capture.normalize(raw)
    def test_symlink_and_public_directory_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp).resolve()
            target = base/'target';target.mkdir(mode=0o700)
            link = base/'link';link.symlink_to(target, target_is_directory=True)
            with self.assertRaises(OSError):capture.directory(str(link))
            target.chmod(0o755)
            with self.assertRaises(ValueError):capture.directory(str(target))

if __name__ == '__main__':unittest.main()
