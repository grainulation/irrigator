#!/usr/bin/env python3
"""Repository consistency checks that are cheap and independent of Go builds.

These guard decisions rather than wording: the native macOS tree stays removed,
CI does not call it, the minimum Go version agrees with go.mod, and the tagged
integration tests have an explicit target. Standard library only.
"""
import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ACTIVE_DOC_GLOBS = ('README.md', 'AGENTS.md', 'CONTRIBUTING.md', 'SECURITY.md',
                    'docs/*.md', 'skills/*/SKILL.md')


def text(path):
    return (ROOT / path).read_text(encoding='utf-8')


def go_minimum():
    match = re.search(r'^go\s+(\d+\.\d+)(?:\.\d+)?\s*$', text('go.mod'), re.M)
    assert match, 'go.mod has no go directive'
    return match.group(1)


def active_docs():
    for pattern in ACTIVE_DOC_GLOBS:
        for path in sorted(ROOT.glob(pattern)):
            yield path


class RepositoryConsistency(unittest.TestCase):
    def test_native_tree_and_retired_tools_are_gone(self):
        for path in ('native', 'tools/debug'):
            target = ROOT / path
            leftovers = [p for p in target.rglob('*') if p.is_file()] if target.is_dir() else []
            self.assertEqual(leftovers, [], f'{path} should have been removed')
        for path in ('tools/debug-launch', 'tools/package_native.py',
                     'tools/audit_native_licenses.py', 'tools/test_package_native.py'):
            self.assertFalse((ROOT / path).exists(), f'{path} should have been removed')

    def test_workflows_do_not_invoke_native_tree(self):
        for workflow in sorted((ROOT / '.github/workflows').glob('*.yml')):
            self.assertNotIn('native/macos', workflow.read_text(encoding='utf-8'), workflow.name)

    def test_active_docs_do_not_link_into_removed_native_tree(self):
        link = re.compile(r'\]\([^)]*native/macos')
        for path in active_docs():
            self.assertIsNone(link.search(path.read_text(encoding='utf-8')),
                              f'{path.relative_to(ROOT)} links into the removed native tree')

    def test_skills_do_not_instruct_removed_panel_or_app(self):
        for path in sorted(ROOT.glob('skills/*/SKILL.md')):
            body = path.read_text(encoding='utf-8')
            self.assertNotRegex(body, r'irrigator show swarm', path.name)
            self.assertNotRegex(body, r'panel --(?:run|follow-session|app-session|display-metadata)', path.name)

    def test_minimum_go_matches_module(self):
        minimum = go_minimum()
        workflow = text('.github/workflows/platforms.yml')
        job = workflow.split('minimum-go:', 1)[1]
        match = re.search(r"go-version:\s*'(\d+\.\d+)(?:\.\d+)?'", job)
        self.assertIsNotNone(match, 'minimum-go job has no go-version')
        self.assertEqual(match.group(1), minimum)
        for name in ('README.md', 'AGENTS.md'):
            self.assertIn(f'Go {minimum}', text(name), f'{name} should state Go {minimum}')

    def test_integration_tests_have_explicit_target_and_ci_step(self):
        self.assertRegex(text('Makefile'), r'(?m)^check-integration:')
        self.assertIn('-tags integration', text('Makefile'))
        self.assertIn('check-integration', text('.github/workflows/platforms.yml'))


if __name__ == '__main__':
    unittest.main()
