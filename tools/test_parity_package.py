#!/usr/bin/env python3
'''Parity tests for tools/package-plugin.py: skill set, binary identity, spaces in paths,
source identity cross-check and hostile archives. Standard library only; builds every
fixture in a temporary directory and never touches the network, a host or a real binary.

Run from a checkout: python3 -I tools/test_parity_package.py
'''
import contextlib
import hashlib
import importlib.util
import io
import json
import os
import re
import shutil
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

NL = chr(10)
HERE = Path(__file__).resolve().parent
TREE = HERE.parent
SPEC = importlib.util.spec_from_file_location('package_plugin', str(HERE / 'package-plugin.py'))
pp = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(pp)

SKILLS = ('irrigator', 'router', 'plan', 'run', 'resume', 'observe', 'budget', 'swarm', 'debug')
VERSION = '1.2.3'
PREFIX = 'irrigator-plugin-' + VERSION
PAYLOAD = b'unique-binary-payload-0123456789'


def run(argv):
    out, err = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = pp.main([str(a) for a in argv])
    return code, out.getvalue(), err.getvalue()


def make_tree(base, skills=SKILLS):
    root = Path(base) / 'consumer root with spaces'
    for name in skills:
        d = root / 'skills' / name
        d.mkdir(parents=True)
        text = '---' + NL + 'name: ' + name + NL + 'description: fixture ' + name + NL + '---' + NL + 'body' + NL
        (d / 'SKILL.md').write_text(text, encoding='utf-8')
    manifest = json.dumps({'name': 'irrigator', 'version': VERSION, 'skills': './skills/'})
    (root / 'plugin.json').write_text(manifest + NL, encoding='utf-8')
    (root / 'README.md').write_text('# fixture' + NL, encoding='utf-8')
    (root / 'docs').mkdir()
    (root / 'docs' / 'note.md').write_text('note' + NL, encoding='utf-8')
    (root / 'main.go').write_text('package main' + NL, encoding='utf-8')
    (root / 'go.mod').write_text('module example' + NL, encoding='utf-8')
    (root / 'docs' / 'plan.schema.json').write_text('{"title":"fixture"}' + NL, encoding='utf-8')
    return root


def fake_binary(base):
    d = Path(base) / 'binary dir'
    d.mkdir(exist_ok=True)
    p = d / 'irrigator'
    p.write_bytes(bytes.fromhex('7f454c46') + PAYLOAD)
    p.chmod(0o755)
    return p


class PackageParity(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix='irrigator parity ')
        self.addCleanup(tmp.cleanup)
        self.base = Path(tmp.name).resolve()

    def build(self, root, out_name='out dir with spaces', extra=()):
        out = self.base / out_name
        code, _, err = run(['--root', root, '--out', out] + list(extra))
        self.assertEqual(code, 0, err)
        return out

    def release(self, out):
        return json.loads((out / 'release-manifest.json').read_text(encoding='utf-8'))

    def test_required_skills_include_swarm_and_debug(self):
        self.assertTrue({'swarm', 'debug'} <= set(pp.REQUIRED_SKILLS))

    def test_missing_swarm_or_debug_skill_fails(self):
        for missing in ('swarm', 'debug'):
            with self.subTest(missing=missing):
                base = self.base / ('without-' + missing)
                base.mkdir()
                root = make_tree(base, tuple(s for s in SKILLS if s != missing))
                code, _, err = run(['--root', root, '--check'])
                self.assertEqual(code, 1)
                self.assertIn('skills/%s/SKILL.md' % missing, err)

    def test_paths_with_spaces_build_and_verify(self):
        root = make_tree(self.base)
        out = self.build(root)
        archive = out / (PREFIX + '.zip')
        self.assertTrue(archive.is_file())
        code, _, err = run(['--verify', archive, '--require-sidecars'])
        self.assertEqual(code, 0, err)

    def test_build_is_deterministic(self):
        root = make_tree(self.base)
        first = self.build(root, 'first out')
        second = self.build(root, 'second out')
        self.assertEqual((first / 'SHA256SUMS').read_bytes(), (second / 'SHA256SUMS').read_bytes())

    def test_source_tree_digest_matches_archive_contents(self):
        root = make_tree(self.base)
        out = self.build(root)
        rep = pp.Report()
        members = pp.read_archive(out / (PREFIX + '.zip'), rep)
        self.assertEqual(rep.errors, [])
        contents = dict((name.split('/', 1)[1], data) for name, data in members)
        self.assertEqual(self.release(out)['source_tree_sha256'], pp.source_tree_digest(contents))

    def test_without_binary_records_none(self):
        root = make_tree(self.base)
        rel = self.release(self.build(root))
        self.assertIsNone(rel['runtime_binary'])
        self.assertIsNone(rel['go_source_sha256'])
        self.assertIs(rel['runtime_binary_bundled'], False)

    def test_editor_schema_is_packaged(self):
        root = make_tree(self.base)
        out = self.build(root)
        members = pp.read_archive(out / (PREFIX + '.zip'), pp.Report())
        self.assertTrue(any(name.endswith('/docs/plan.schema.json') for name, _ in members))

    def test_source_digest_tracks_relocated_runtime(self):
        root = make_tree(self.base)
        before = pp.go_source_digest(root, pp.Report())
        (root / 'main.go').write_text('package main\n// changed runtime\n', encoding='utf-8')
        after = pp.go_source_digest(root, pp.Report())
        self.assertNotEqual(before, after)

    def test_binary_identity_recorded_but_not_bundled(self):
        root = make_tree(self.base)
        binary = fake_binary(self.base)
        out = self.build(root, extra=['--binary', binary])
        rel = self.release(out)
        info = rel['runtime_binary']
        self.assertEqual(info['sha256'], hashlib.sha256(bytes.fromhex('7f454c46') + PAYLOAD).hexdigest())
        self.assertEqual(info['name'], 'irrigator')
        self.assertIs(info['bundled'], False)
        self.assertIs(info['executed_by_packager'], False)
        self.assertIs(info['built_from_go_source_verified'], False)
        self.assertIs(rel['runtime_binary_bundled'], False)
        self.assertRegex(rel['go_source_sha256'], '^[0-9a-f]{64}$')
        self.assertNotIn(str(binary.parent), (out / 'release-manifest.json').read_text(encoding='utf-8'))
        members = pp.read_archive(out / (PREFIX + '.zip'), pp.Report())
        for name, data in members:
            self.assertNotIn(PAYLOAD, data, name)
        code, _, err = run(['--verify', out / (PREFIX + '.zip'), '--require-sidecars'])
        self.assertEqual(code, 0, err)

    def test_unsafe_binary_arguments_are_refused(self):
        root = make_tree(self.base)
        good = fake_binary(self.base)
        link = self.base / 'binary-link'
        os.symlink(str(good), str(link))
        plain = self.base / 'not-executable'
        plain.write_bytes(PAYLOAD)
        plain.chmod(0o644)
        inside_dir = root / 'bin dir'
        inside_dir.mkdir()
        inside = inside_dir / 'irrigator'
        shutil.copy(str(good), str(inside))
        inside.chmod(0o755)
        cases = {'relative': 'irrigator', 'symlink': link, 'not executable': plain, 'inside tree': inside,
                 'missing': self.base / 'does not exist'}
        for label, binary in cases.items():
            with self.subTest(case=label):
                code, _, err = run(['--root', root, '--check', '--binary', binary])
                self.assertEqual(code, 1, err)
                self.assertIn('--binary', err)

    def test_verify_rejects_binary_flag(self):
        root = make_tree(self.base)
        out = self.build(root)
        code, _, _ = run(['--verify', out / (PREFIX + '.zip'), '--binary', fake_binary(self.base)])
        self.assertEqual(code, 2)

    def test_tampered_source_identity_is_detected(self):
        root = make_tree(self.base)
        out = self.build(root)
        archive = out / (PREFIX + '.zip')
        code, _, err = run(['--verify', archive, '--require-sidecars'])
        self.assertEqual(code, 0, err)
        path = out / 'release-manifest.json'
        rel = self.release(out)
        rel['source_tree_sha256'] = 'f' * 64
        blob = (json.dumps(rel, indent=2, sort_keys=True) + NL).encode('utf-8')
        path.write_bytes(blob)
        sums = '%s  %s%s%s  %s%s' % (pp.sha256_file(archive), archive.name, NL, pp.sha256_bytes(blob), path.name, NL)
        (out / 'SHA256SUMS').write_text(sums, encoding='utf-8')
        code, _, err = run(['--verify', archive, '--require-sidecars'])
        self.assertEqual(code, 1)
        self.assertIn('source_tree_sha256', err)

    def write_zip(self, name, members):
        path = self.base / name
        with zipfile.ZipFile(str(path), 'w') as zf:
            for member, data in members:
                zf.writestr(member, data)
        return path

    def test_traversal_archive_is_rejected(self):
        path = self.write_zip('traversal.zip', [(PREFIX + '/../evil.md', 'x')])
        code, _, err = run(['--verify', path])
        self.assertEqual(code, 1)
        self.assertIn('unsafe member name', err)

    def test_case_colliding_archive_is_rejected(self):
        path = self.write_zip('collide.zip', [(PREFIX + '/docs/a.md', 'x'), (PREFIX + '/docs/A.md', 'y')])
        code, _, err = run(['--verify', path])
        self.assertEqual(code, 1)
        self.assertIn('collision', err)

    def test_symlink_member_archive_is_rejected(self):
        path = self.base / 'link.tar.gz'
        with tarfile.open(str(path), 'w:gz') as tf:
            info = tarfile.TarInfo(PREFIX + '/plugin.json')
            info.type = tarfile.SYMTYPE
            info.linkname = '/etc/hosts'
            tf.addfile(info)
        code, _, err = run(['--verify', path])
        self.assertEqual(code, 1)
        self.assertIn('non-regular', err)

    def test_first_use_docs_have_no_home_paths_or_managed_cli(self):
        bad = re.compile(r'irrigator[ ]+managed|managed[.]go|/Users/|/home/[a-z]')
        checked = 0
        for rel in ('README.md', 'docs/plugin-packaging.md'):
            path = TREE / rel
            if not path.is_file():
                continue
            checked += 1
            self.assertIsNone(bad.search(path.read_text(encoding='utf-8')), rel)
        if not checked:
            self.skipTest('no first-use docs in this tree')

    def test_first_use_docs_name_binary_flag(self):
        guide = TREE / 'docs' / 'plugin-packaging.md'
        if not guide.is_file():
            self.skipTest('docs/plugin-packaging.md not present')
        self.assertIn('--binary', guide.read_text(encoding='utf-8'))

    def test_allowlist_names_packaging_docs_and_parity_test(self):
        self.assertIn('tools/test_parity_package.py', pp.INCLUDE_FILES)
        self.assertTrue(pp.allowed_rel('tools/test_parity_package.py'))
        self.assertTrue(pp.allowed_rel('docs/plugin-packaging.md'))

    def test_consumer_archive_carries_packaging_docs_and_parity_test(self):
        root = make_tree(self.base)
        wanted = ('docs/plugin-packaging.md', 'tools/test_parity_package.py', 'tools/package-plugin.py')
        for rel in wanted:
            path = root / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('# fixture ' + rel + NL, encoding='utf-8')
        out = self.build(root)
        archive = out / (PREFIX + '.zip')
        rep = pp.Report()
        members = pp.read_archive(archive, rep)
        self.assertEqual(rep.errors, [])
        names = set(name.split('/', 1)[1] for name, _ in members)
        for rel in wanted:
            self.assertIn(rel, names)
        code, _, err = run(['--verify', archive, '--require-sidecars'])
        self.assertEqual(code, 0, err)

    def test_local_release_doc_counts_nine_skills_and_separates_invocation(self):
        doc = TREE / 'docs' / 'local-release.md'
        if not doc.is_file():
            self.skipTest('docs/local-release.md not present')
        text = doc.read_text(encoding='utf-8')
        self.assertNotIn('seven', text.lower())
        self.assertIn('nine skills', text)
        for name in ('swarm', 'debug'):
            self.assertIn('`' + name + '`', text)
        self.assertIn('Discovery is not model invocation', text)


if __name__ == '__main__':
    unittest.main()
