"""Stopping a swarm keeps its work; resuming repeats none of it.

python3 verification/swarm-durability-test.py   (needs ./irrigator; mock workers, no model calls)
"""
from collections import Counter
import json
from pathlib import Path
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
RUNTIME = ROOT / 'irrigator'
PLAN = ROOT / 'examples' / 'terminal-swarm.json'


def status(run):
    return json.loads(subprocess.run([str(RUNTIME), 'status', str(run), '--json'], check=True, capture_output=True, text=True).stdout)


def main():
    with tempfile.TemporaryDirectory(prefix='irrigator-durability-') as temporary:
        run = Path(temporary) / 'run'
        subprocess.run([str(RUNTIME), 'prepare', str(PLAN), '--run', str(run), '--backend', 'mock'], check=True, capture_output=True)
        worker = subprocess.Popen([str(RUNTIME), 'resume', str(run), '--workers', '1'], env={'IRRIGATOR_UI': 'plain', 'PATH': '/usr/bin:/bin'},
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        # Stop only after real progress, so the check is meaningful.
        deadline = time.time() + 30
        while time.time() < deadline:
            done = sum(1 for t in status(run)['tasks'].values() if t and t.get('status') == 'verified')
            if done >= 2 or worker.poll() is not None:
                break
            time.sleep(0.05)
        subprocess.run([str(RUNTIME), 'down', str(run)], check=True, capture_output=True)
        worker.wait(timeout=60)
        before = status(run)
        verified = [k for k, t in before['tasks'].items() if t and t.get('status') == 'verified']
        started = Counter(e.get('task') for e in before.get('events', []) if e.get('kind') == 'started')
        assert before['control'] == 'down', before['control']
        assert len(verified) >= 2, 'run finished or stalled before it could be stopped mid-way (%d verified)' % len(verified)
        assert len(verified) < len(before['tasks']), 'run completed before the stop; nothing left to resume'
        subprocess.run([str(RUNTIME), 'resume', str(run), '--workers', '3'], check=True, capture_output=True, env={'IRRIGATOR_UI': 'plain', 'PATH': '/usr/bin:/bin'})
        after = status(run)
        again = Counter(e.get('task') for e in after.get('events', []) if e.get('kind') == 'started')
        repeated = [k for k in verified if again[k] != started[k]]
        assert not repeated, 'verified tasks ran again after resume: %s' % repeated
        remaining = [k for k, t in after['tasks'].items() if t and t.get('status') not in ('verified', 'failed', 'uncertain', 'cancelled')]
        print('PASS: stop kept %d verified tasks; resume finished the rest (%d left unfinished) without repeating any' % (len(verified), len(remaining)))


if __name__ == '__main__':
    main()
