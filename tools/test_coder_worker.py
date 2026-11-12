"""Exercise examples/coder/worker.py with a fake `claude` on PATH: no network, no model."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKER = ROOT / 'examples/coder/worker.py'
PLAN = ROOT / 'examples/coder/plan.py'
CLEANUP = ROOT / 'examples/coder/cleanup.py'

FAKE_CLAUDE = '''#!{python}
import json, os, subprocess, sys, time
mode = os.environ["FAKE_CLAUDE_MODE"]
with open(os.environ["FAKE_CLAUDE_ARGS"], "w") as f:
    json.dump({{"argv": sys.argv[1:], "cwd": os.getcwd()}}, f)
def emit(ev):
    print(json.dumps(ev), flush=True)
emit({{"type": "assistant", "message": {{"content": [{{"type": "tool_use", "name": "Write", "input": {{"file_path": "hello.txt"}}}}]}}}})
if mode == "timeout":
    time.sleep(60)
if mode == "noresult":
    sys.exit(1)
if mode in ("success", "leftover"):
    open("hello.txt", "w").write("hi\\n")
if mode == "second":
    open("second.txt", "w").write("two\\n")
if mode == "success":
    subprocess.run(["git", "add", "hello.txt"], check=True)
    subprocess.run(["git", "commit", "-q", "-m", "add hello"], check=True)
if mode == "second":
    subprocess.run(["git", "add", "second.txt"], check=True)
    subprocess.run(["git", "commit", "-q", "-m", "add second"], check=True)
LIMIT = "Claude AI usage limit reached. Your session limit resets 3pm (America/New_York)"
text = {{"error": "agent says: boom", "limit": LIMIT}}.get(mode, "added hello.txt")
emit({{"type": "result", "is_error": mode in ("error", "limit"), "result": text,
      "total_cost_usd": 0.0123,
      "usage": {{"input_tokens": 120, "output_tokens": 40}},
      "model": "claude-3-5-sonnet-20241022"}})
'''


class CoderWorkerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve()
        self.repo = self.root / 'repo'
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.args_file = self.root / 'claude-args.json'
        claude = self.bin / 'claude'
        claude.write_text(FAKE_CLAUDE.format(python=sys.executable))
        claude.chmod(0o755)
        self.env = {**os.environ, 'PATH': f'{self.bin}{os.pathsep}{os.environ["PATH"]}',
                    'FAKE_CLAUDE_ARGS': str(self.args_file),
                    'GIT_AUTHOR_NAME': 'T', 'GIT_AUTHOR_EMAIL': 't@example.com',
                    'GIT_COMMITTER_NAME': 'T', 'GIT_COMMITTER_EMAIL': 't@example.com'}
        self.git('init', '-q', '-b', 'main', str(self.repo), cwd=self.root)
        (self.repo / 'README.md').write_text('base\n')
        self.git('add', '-A')
        self.git('commit', '-q', '-m', 'base')

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args, cwd=None):
        return subprocess.run(['git', *args], cwd=cwd or self.repo, env=self.env, check=True,
                              capture_output=True, text=True).stdout.strip()

    def run_worker(self, mode, base='main', timeout_ms=1800000, kind='task', requires=None, attempt=1,
                   context=None, run_id='0123456789abcdef', extra=()):
        self.env['FAKE_CLAUDE_MODE'] = mode
        request = {'protocol': 'swarm.worker/v1', 'kind': kind, 'request_id': 'req-1', 'operation_id': 'op-1',
                   'run_id': run_id, 'attempt': attempt, 'timeout_ms': timeout_ms, 'max_cost_usd': 0.5,
                   'task': {'id': 'cell1', 'prompt': 'Add hello.txt', 'requires': requires or []},
                   'context': {'notes': 'be brief', **(context or {})}}
        done = subprocess.run([sys.executable, '-I', str(WORKER), '--repo', str(self.repo), '--base', base,
                               '--worktrees', str(self.root / 'wt'), '--no-tty', *extra],
                              input=json.dumps(request), env=self.env, capture_output=True, text=True, timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        response = json.loads(done.stdout)
        self.assertEqual(response['protocol'], 'swarm.worker/v1')
        self.assertEqual(response['request_id'], 'req-1')
        return response

    def test_success_lands_a_branch_in_a_worktree(self):
        response = self.run_worker('success')
        self.assertEqual(response['outcome'], 'settled')
        self.assertNotIn('error', response)
        self.assertEqual(response['cost_usd'], 0.0123)
        self.assertEqual(response['usage'], {'input_tokens': 120, 'output_tokens': 40, 'thinking_tokens': 0,
                                             'cache_read_tokens': 0, 'cache_write_tokens': 0, 'total_tokens': 160,
                                             'model': 'claude-3-5-sonnet-20241022'})
        self.assertEqual(response['model'], 'claude-3-5-sonnet-20241022')
        out = response['output']
        self.assertEqual(out['branch'], 'irrigator/01234567/cell1')
        self.assertEqual(out['commit'], self.git('rev-parse', out['branch']))
        self.assertIn('1 file changed', out['diffstat'])
        self.assertEqual(out['summary'], 'added hello.txt')
        self.assertEqual(self.git('show', f'{out["branch"]}:hello.txt'), 'hi')
        recorded = json.loads(self.args_file.read_text())
        self.assertEqual(Path(recorded['cwd']).resolve(), (self.root / 'wt/01234567/cell1-a1').resolve())
        argv = recorded['argv']
        self.assertEqual(argv[argv.index('--max-budget-usd') + 1], '0.50')
        # The user's own Claude settings (allow rules, hooks) never apply to a cell.
        self.assertEqual(argv[argv.index('--setting-sources') + 1], '')
        denied = argv[argv.index('--disallowedTools') + 1].split(',')
        for rule in ('Bash(git push:*)', 'Bash(git reset:*)', 'Bash(git checkout:*)', 'Bash(git switch:*)', 'Bash(rm:*)',
                     'WebFetch', 'WebSearch'):
            self.assertIn(rule, denied)
        allowed = argv[argv.index('--allowedTools') + 1].split(',')
        for tool in ('python3', 'node', 'npm', 'npx', 'cat', 'head', 'tail', 'wc', 'grep', 'find', 'diff', 'ls',
                     'mkdir', 'cp', 'mv', 'chmod', 'git show', 'git mv'):
            self.assertIn(f'Bash({tool}:*)', allowed)
        for rule in denied:
            self.assertNotIn(rule, allowed)
        self.assertNotIn('--add-dir', argv)
        self.assertIn('be brief', argv[argv.index('-p') + 1])

    def test_requires_selects_output_fields(self):
        response = self.run_worker('success', requires=['branch', 'summary'])
        self.assertEqual(sorted(response['output']), ['branch', 'summary'])

    def test_leftover_changes_are_committed(self):
        response = self.run_worker('leftover')
        self.assertEqual(response['outcome'], 'settled')
        self.assertEqual(self.git('show', f'{response["output"]["branch"]}:hello.txt'), 'hi')
        self.assertIn('uncommitted agent changes', self.git('log', '-1', '--format=%s', response['output']['branch']))

    def test_agent_error_is_settled_with_error(self):
        response = self.run_worker('error')
        self.assertEqual(response['outcome'], 'settled')
        self.assertIn('agent error: agent says: boom', response['error'])
        self.assertNotIn('output', response)

    def test_timeout_is_uncertain(self):
        response = self.run_worker('timeout', timeout_ms=31000)
        self.assertEqual(response['outcome'], 'uncertain')
        self.assertIn('timed out', response['error'])

    def test_missing_result_is_uncertain(self):
        self.assertEqual(self.run_worker('noresult')['outcome'], 'uncertain')

    def test_setup_failure_is_not_started(self):
        response = self.run_worker('success', base='no-such-branch')
        self.assertEqual(response['outcome'], 'not_started')
        self.assertIn('worktree setup failed', response['error'])
        self.assertFalse(self.args_file.exists(), 'claude must not run after setup fails')

    def test_validator_request_is_not_started(self):
        self.assertEqual(self.run_worker('success', kind='validator')['outcome'], 'not_started')

    def test_missing_claude_is_not_started(self):
        (self.bin / 'claude').unlink()
        self.env['PATH'] = str(self.bin)  # git is found through the absolute path below
        git = subprocess.run(['which', 'git'], capture_output=True, text=True).stdout.strip()
        os.symlink(git, self.bin / 'git')
        response = self.run_worker('success')
        self.assertEqual(response['outcome'], 'not_started')
        self.assertIn('could not launch claude', response['error'])

    def prompt(self):
        argv = json.loads(self.args_file.read_text())['argv']
        return argv[argv.index('-p') + 1]

    def worktree(self, attempt):
        return self.root / 'wt/01234567' / f'cell1-a{attempt}'

    def run_cleanup(self, *extra):
        return subprocess.run([sys.executable, '-I', str(CLEANUP), '--repo', str(self.repo), '--run', '0123456789abcdef', *extra],
                              env=self.env, capture_output=True, text=True)

    def test_retry_retires_clean_worktree_and_continues_branch(self):
        self.assertEqual(self.run_worker('success')['outcome'], 'settled')
        self.assertNotIn('previous_attempts', self.prompt())
        self.assertTrue(self.worktree(1).exists())
        response = self.run_worker('second', attempt=2)
        self.assertEqual(response['outcome'], 'settled', response)
        self.assertFalse(self.worktree(1).exists(), 'clean earlier worktree should be removed')
        self.assertTrue(self.worktree(2).exists())
        branch = response['output']['branch']
        self.assertEqual(self.git('show', f'{branch}:hello.txt'), 'hi')
        self.assertEqual(self.git('show', f'{branch}:second.txt'), 'two')

    def test_retry_with_commits_adds_previous_attempts_section(self):
        self.run_worker('success')
        self.run_worker('second', attempt=2)
        prompt = self.prompt()
        self.assertIn('## Context: previous_attempts', prompt)
        self.assertIn('already has 1 commit(s) beyond main', prompt)
        self.assertIn('add hello', prompt)
        self.assertIn('rather than starting over', prompt)

    def test_retry_with_no_commits_has_no_previous_attempts_section(self):
        self.run_worker('error')  # agent error leaves the branch at base
        self.run_worker('success', attempt=2)
        self.assertNotIn('previous_attempts', self.prompt())

    def test_retry_refuses_dirty_earlier_worktree(self):
        self.run_worker('success')
        (self.worktree(1) / 'wip.txt').write_text('unsaved\n')
        self.args_file.unlink()
        response = self.run_worker('second', attempt=2)
        self.assertEqual(response['outcome'], 'not_started')
        self.assertIn(str(self.worktree(1)), response['error'])
        self.assertIn('uncommitted', response['error'])
        self.assertTrue((self.worktree(1) / 'wip.txt').exists(), 'dirty work must survive')
        self.assertFalse(self.args_file.exists(), 'claude must not run')

    def test_cleanup_removes_worktrees_but_keeps_branches_by_default(self):
        branch = self.run_worker('success')['output']['branch']
        done = self.run_cleanup()
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn('removed', done.stdout)
        self.assertFalse(self.worktree(1).exists())
        self.assertEqual(self.git('branch', '--list', branch).strip(), branch)

    def test_cleanup_dry_run_changes_nothing(self):
        self.run_worker('success')
        done = self.run_cleanup('--dry-run', '--delete-branches')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertIn('would remove', done.stdout)
        self.assertIn('would delete branch', done.stdout)
        self.assertTrue(self.worktree(1).exists())

    def test_cleanup_delete_branches(self):
        branch = self.run_worker('success')['output']['branch']
        done = self.run_cleanup('--delete-branches')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(self.git('branch', '--list', branch), '')

    def test_cleanup_refuses_dirty_worktree_unless_forced(self):
        branch = self.run_worker('success')['output']['branch']
        (self.worktree(1) / 'wip.txt').write_text('unsaved\n')
        done = self.run_cleanup('--delete-branches')
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('refuse', done.stdout)
        self.assertTrue((self.worktree(1) / 'wip.txt').exists())
        self.assertNotEqual(self.git('branch', '--list', branch), '', 'branch kept with its worktree')
        forced = self.run_cleanup('--delete-branches', '--force')
        self.assertEqual(forced.returncode, 0, forced.stderr)
        self.assertFalse(self.worktree(1).exists())
        self.assertEqual(self.git('branch', '--list', branch), '')

    def test_cleanup_ignores_other_runs(self):
        self.run_worker('success')
        other = subprocess.run([sys.executable, '-I', str(CLEANUP), '--repo', str(self.repo), '--run', 'ffffffff'],
                               env=self.env, capture_output=True, text=True)
        self.assertEqual(other.returncode, 0, other.stderr)
        self.assertTrue(self.worktree(1).exists())

    def test_verify_pass_adds_note_and_runs_in_worktree(self):
        response = self.run_worker('success', context={'verify_cell1': 'test -f hello.txt && pwd > verified.txt'})
        self.assertEqual(response['outcome'], 'settled', response)
        self.assertNotIn('error', response)
        self.assertIn('Verification `test -f hello.txt && pwd > verified.txt` passed.', response['output']['summary'])
        self.assertTrue((self.worktree(1) / 'verified.txt').exists())

    def test_verify_failure_is_settled_with_output_tail(self):
        response = self.run_worker('success', context={'verify_cell1': 'echo nope-output; echo bad-err >&2; exit 3'})
        self.assertEqual(response['outcome'], 'settled')
        self.assertNotIn('output', response)
        self.assertIn('verification failed (exit 3)', response['error'])
        self.assertIn('nope-output', response['error'])
        self.assertIn('bad-err', response['error'])

    def test_verify_key_replaces_dash_in_cell_id(self):
        self.env['FAKE_CLAUDE_MODE'] = 'success'
        request = {'protocol': 'swarm.worker/v1', 'kind': 'task', 'request_id': 'r', 'run_id': '0123456789abcdef',
                   'attempt': 1, 'timeout_ms': 1800000, 'max_cost_usd': 0.5,
                   'task': {'id': 'my-cell', 'prompt': 'x', 'requires': []}, 'context': {'verify_my_cell': 'exit 1'}}
        done = subprocess.run([sys.executable, '-I', str(WORKER), '--repo', str(self.repo), '--worktrees',
                               str(self.root / 'wt'), '--no-tty'], input=json.dumps(request), env=self.env,
                              capture_output=True, text=True, timeout=60)
        self.assertIn('verification failed (exit 1)', json.loads(done.stdout)['error'])

    def test_verify_skipped_when_agent_errors_or_makes_no_commits(self):
        marker = self.root / 'ran'
        context = {'verify_cell1': f'touch {marker}'}
        self.assertIn('agent error', self.run_worker('error', context=context)['error'])
        self.assertIn('without changing anything', self.run_worker('nochange', attempt=2, context=context)['error'])
        self.assertFalse(marker.exists())

    def test_add_dir_is_passed_to_claude(self):
        ref = self.root / 'ref'
        ref.mkdir()
        self.run_worker('success', extra=['--add-dir', str(ref), '--add-dir', str(self.root)])
        argv = json.loads(self.args_file.read_text())['argv']
        pairs = [argv[i + 1] for i, a in enumerate(argv) if a == '--add-dir']
        self.assertEqual(pairs, [str(ref), str(self.root)])

    def test_continue_from_builds_on_earlier_runs_branch(self):
        self.assertEqual(self.run_worker('success')['outcome'], 'settled')
        response = self.run_worker('second', run_id='fedcba9876543210', extra=['--continue-from', '0123456789abcdef'])
        self.assertEqual(response['outcome'], 'settled', response)
        branch = response['output']['branch']
        self.assertEqual(branch, 'irrigator/fedcba98/cell1')
        self.assertEqual(self.git('show', f'{branch}:hello.txt'), 'hi')
        self.assertEqual(self.git('show', f'{branch}:second.txt'), 'two')
        prompt = self.prompt()
        self.assertIn('## Context: previous_attempts', prompt)
        self.assertIn('add hello', prompt)
        self.assertIn('irrigator/01234567/cell1', prompt)

    def test_continue_from_without_earlier_branch_starts_at_base(self):
        response = self.run_worker('success', run_id='fedcba9876543210', extra=['--continue-from', 'aaaaaaaa'])
        self.assertEqual(response['outcome'], 'settled', response)
        self.assertNotIn('previous_attempts', self.prompt())
        self.assertEqual(self.git('rev-list', '--count', 'main..irrigator/fedcba98/cell1'), '1')

    def test_continue_from_ignored_when_cell_branch_exists(self):
        self.run_worker('success')
        self.run_worker('success', run_id='fedcba9876543210', extra=['--continue-from', '0123456789abcdef'])
        response = self.run_worker('second', run_id='fedcba9876543210', attempt=2,
                                   extra=['--continue-from', '0123456789abcdef'])
        self.assertEqual(response['outcome'], 'settled', response)

    def test_usage_limit_error_is_plain_and_has_reset_time(self):
        response = self.run_worker('limit')
        self.assertEqual(response['outcome'], 'settled')
        self.assertNotIn('output', response)
        error = response['error']
        self.assertIn('usage limit', error)
        self.assertIn('resets 3pm (America/New_York)', error)
        self.assertIn('not a code problem', error)
        self.assertNotIn('agent error', error)

    def test_plan_generator_verify_add_dir_and_continue_from(self):
        ref = self.root / 'ref'
        ref.mkdir()
        done = subprocess.run([sys.executable, '-I', str(PLAN), '--repo', str(self.repo), '--task', 'my-cell=Do A',
                               '--task', 'b=Do B', '--verify', 'my-cell=make test && echo ok=1', '--add-dir', str(ref),
                               '--continue-from', '0123456789abcdef'], capture_output=True, text=True, check=True)
        plan = json.loads(done.stdout)
        self.assertEqual(plan['context'], {'verify_my_cell': 'make test && echo ok=1'})
        self.assertEqual(plan['tasks'][0]['context'], ['verify_my_cell'])
        self.assertNotIn('context', plan['tasks'][1])
        command = plan['providers']['coder']['command']
        self.assertEqual(command[command.index('--add-dir') + 1], str(ref.resolve()))
        self.assertEqual(command[command.index('--continue-from') + 1], '0123456789abcdef')
        bad = subprocess.run([sys.executable, '-I', str(PLAN), '--repo', str(self.repo), '--task', 'a=x',
                              '--verify', 'zzz=true'], capture_output=True, text=True)
        self.assertNotEqual(bad.returncode, 0)

    def test_plan_generator(self):
        done = subprocess.run([sys.executable, '-I', str(PLAN), '--repo', str(self.repo), '--base', 'main', '--model', 'haiku',
                               '--task', 'a=Do A', '--task', 'b=Do B', '--depends', 'b=a'],
                              capture_output=True, text=True, check=True)
        plan = json.loads(done.stdout)
        self.assertEqual(plan['providers']['coder']['env'], ['USER'])
        self.assertEqual(plan['max_tasks'], 2)
        command = plan['providers']['coder']['command']
        self.assertEqual(command[command.index('--model') + 1], 'haiku')
        self.assertEqual(plan['tasks'][1]['depends_on'], ['a'])
        bad = subprocess.run([sys.executable, '-I', str(PLAN), '--repo', str(self.repo)], capture_output=True, text=True)
        self.assertNotEqual(bad.returncode, 0)


if __name__ == '__main__':
    unittest.main()
