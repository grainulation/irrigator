#!/usr/bin/env python3
'''Regression tests for the safety behavior of tools/package-plugin.py.

Run (stdlib only, local only):

    python3 -I verification/package-safety-test.py [-v]

Every test builds its fixtures in a fresh temporary directory. Nothing is written
into the plugin tree, installed into a host, pushed, published or sent over a
network. Symlink tests skip themselves where the platform cannot create links.

Covered: symlinked parent components of fixed and discovered sources, planted
archive and sidecar .part links (sentinel targets must stay unchanged), sidecar
symlinks and non-echo of sidecar content, --require-sidecars, secret filenames,
unsafe manifest path fields versus legitimate public text, malicious archive
prefix and Unicode collisions, and a clean deterministic build and verify.

Run against the current packager before accepting a release artifact.
'''
import contextlib
import errno
import importlib.util
import io
import json
import os
import shutil
import sys
import tarfile
import tempfile
import unittest
import warnings
import zipfile
from pathlib import Path
from unittest import mock

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
_SPEC = importlib.util.spec_from_file_location('package_plugin', str(ROOT / 'tools' / 'package-plugin.py'))
P = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(P)

NL = chr(10)
EPOCH = '1700000000'
VERSION = '1.0.0'
PREFIX = 'irrigator-plugin-' + VERSION
MARKER = 'PRIVATE-MARKER-7f3a9c'
SKILLS = ('irrigator', 'router', 'plan', 'run', 'resume', 'observe', 'budget', 'swarm', 'debug')


def run(argv):
    out, err = io.StringIO(), io.StringIO()
    with mock.patch.dict(os.environ, {'SOURCE_DATE_EPOCH': EPOCH}):
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = P.main([str(a) for a in argv])
    return rc, out.getvalue(), err.getvalue()


def write(root, rel, text):
    path = Path(root) / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(text, str):
        path.write_text(text, encoding='utf-8')
    else:
        path.write_bytes(text)
    return path


def skill_text(name):
    return '---' + NL + 'name: %s' % name + NL + 'description: fixture skill' + NL + '---' + NL + 'Body' + NL


def make_tree(base, extra=None):
    root = Path(base) / 'root'
    data = {'name': 'irrigator', 'version': VERSION, 'description': 'Test fixture',
            'extensions': {'com.openai': {'requires_local_executor': True}}}
    data.update(extra or {})
    write(root, 'plugin.json', json.dumps(data, indent=2) + NL)
    write(root, 'README.md', '# Fixture' + NL)
    for skill in SKILLS:
        write(root, 'skills/%s/SKILL.md' % skill, skill_text(skill))
    return root


def link(case, target, where):
    try:
        os.symlink(str(target), str(where), target_is_directory=Path(target).is_dir())
    except (NotImplementedError, AttributeError):
        case.skipTest('symlinks unavailable on this platform')
    except OSError as exc:
        if exc.errno in (errno.EPERM, errno.EACCES, errno.ENOSYS, errno.ENOTSUP):
            case.skipTest('symlinks unavailable on this platform')
        raise


def build(case, root, out, fmt='zip'):
    rc, _o, err = run(['--root', root, '--out', out, '--format', fmt])
    case.assertEqual(rc, 0, err)
    return Path(out) / ('%s.%s' % (PREFIX, fmt))


def zip_members(path):
    with zipfile.ZipFile(str(path)) as zf:
        return [(i.filename, zf.read(i.filename)) for i in zf.infolist()]


def write_zip(path, members):
    with warnings.catch_warnings():
        warnings.simplefilter('ignore')
        with zipfile.ZipFile(str(path), 'w') as zf:
            for name, data in members:
                info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
                info.external_attr = 0o100644 << 16
                zf.writestr(info, data)


def write_tar(path, members):
    with tarfile.open(str(path), 'w:gz', format=tarfile.PAX_FORMAT) as tf:
        for name, data in members:
            info = tarfile.TarInfo(name)
            info.size = len(data)
            tf.addfile(info, io.BytesIO(data))


class CleanBuildAndVerify(unittest.TestCase):
    def test_deterministic_build_and_verify(self):
        for fmt in ('zip', 'tar.gz'):
            with tempfile.TemporaryDirectory() as d:
                root = make_tree(d)
                a = build(self, root, Path(d) / 'o1', fmt)
                b = build(self, root, Path(d) / 'o2', fmt)
                self.assertEqual(a.read_bytes(), b.read_bytes(), fmt)
                for name in ('SHA256SUMS', 'release-manifest.json'):
                    self.assertEqual((a.parent / name).read_bytes(), (b.parent / name).read_bytes(), name)
                self.assertEqual(sorted(p.name for p in a.parent.iterdir()),
                                 sorted([a.name, 'SHA256SUMS', 'release-manifest.json']))
                rc, _o, err = run(['--verify', a])
                self.assertEqual(rc, 0, err)
                rc, _o, err = run(['--verify', a, '--require-sidecars'])
                self.assertEqual(rc, 0, err)

    def test_existing_artifacts_need_force(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            a = build(self, root, Path(d) / 'out')
            before = a.read_bytes()
            rc, _o, _e = run(['--root', root, '--out', a.parent])
            self.assertEqual(rc, 2)
            rc, _o, err = run(['--root', root, '--out', a.parent, '--force'])
            self.assertEqual(rc, 0, err)
            self.assertEqual(a.read_bytes(), before)

    def test_allowlist_includes_consumer_scripts_and_swarm_skill(self):
        wanted = ('verification/plugin-contract-test.py', 'verification/plugin-validator-regression.py',
                  'verification/plugin-consumer-test.py', 'skills/swarm/SKILL.md')
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            for rel in wanted[:3]:
                write(root, rel, '# fixture' + NL)
            write(root, 'verification/unlisted.py', '# not packaged' + NL)
            write(root, 'skills/swarm/SKILL.md', skill_text('swarm'))
            rc, out, err = run(['--root', root, '--list'])
            self.assertEqual(rc, 0, err)
            listed = out.splitlines()
            for rel in wanted:
                self.assertIn(rel, listed)
            self.assertNotIn('verification/unlisted.py', listed)
            archive = build(self, root, Path(d) / 'out')
            names = [n for n, _data in zip_members(archive)]
            for rel in wanted:
                self.assertIn(PREFIX + '/' + rel, names)
            rc, _o, err = run(['--verify', archive, '--require-sidecars'])
            self.assertEqual(rc, 0, err)

    def test_require_sidecars_only_with_verify(self):
        rc, _o, _e = run(['--check', '--require-sidecars'])
        self.assertEqual(rc, 2)


class SymlinkParents(unittest.TestCase):
    def test_symlinked_parent_of_fixed_include_files_is_refused(self):
        for parent, name in (('verification', 'plugin-contract-test.py'),
                             ('verification', 'plugin-consumer-test.py'),
                             ('verification', 'plugin-validator-regression.py'),
                             ('tools', 'package-plugin.py')):
            with tempfile.TemporaryDirectory() as d:
                root = make_tree(d)
                outside = Path(d) / 'outside'
                outside.mkdir()
                (outside / name).write_text('OUTSIDE-MARKER' + NL)
                link(self, outside, root / parent)
                rel = parent + '/' + name
                rc, out, err = run(['--root', root, '--check'])
                self.assertEqual(rc, 1, rel)
                self.assertIn('symlink', err)
                rc, out, _e = run(['--root', root, '--list'])
                self.assertNotIn(rel, out.splitlines())
                dist = Path(d) / 'dist'
                rc, _o, _e = run(['--root', root, '--out', dist])
                self.assertEqual(rc, 1)
                self.assertFalse(dist.exists())
                self.assertEqual((outside / name).read_text(), 'OUTSIDE-MARKER' + NL)

    def test_symlinked_docs_and_nested_skill_directories_are_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            outside = Path(d) / 'outside'
            write(outside, 'page.md', 'OUTSIDE' + NL)
            link(self, outside, root / 'docs')
            rc, _o, err = run(['--root', root, '--check'])
            self.assertEqual(rc, 1)
            self.assertIn('symlink', err)
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            outside = Path(d) / 'outside'
            write(outside, 'SKILL.md', skill_text('swarm'))
            shutil.rmtree(root / 'skills' / 'swarm')
            link(self, outside, root / 'skills' / 'swarm')
            rc, _o, err = run(['--root', root, '--check'])
            self.assertEqual(rc, 1)
            self.assertIn('symlink', err)

    def test_read_regular_refuses_symlink_components(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            outside = Path(d) / 'outside'
            write(outside, 'x.py', 'OUTSIDE' + NL)
            link(self, outside, root / 'tools')
            with self.assertRaises(OSError):
                P.read_regular(root, 'tools/x.py')
            self.assertEqual(P.read_regular(root, 'README.md'), ('# Fixture' + NL).encode('utf-8'))

    def test_manifest_reference_through_symlink_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d, {'icon': './link.png'})
            outside = Path(d) / 'outside.png'
            outside.write_bytes(b'x')
            link(self, outside, root / 'link.png')
            rc, _o, err = run(['--root', root, '--check'])
            self.assertEqual(rc, 1)
            self.assertIn('symlink', err)


class PlantedPartPaths(unittest.TestCase):
    def plant_and_build(self, fmt, plant, force, dangling=False):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            out = Path(d) / 'out'
            out.mkdir()
            sentinel = Path(d) / 'sentinel.txt'
            if not dangling:
                sentinel.write_bytes(b'SENTINEL' + NL.encode())
            link(self, sentinel, out / plant)
            argv = ['--root', root, '--out', out, '--format', fmt] + (['--force'] if force else [])
            rc, _o, err = run(argv)
            self.assertEqual(rc, 2, err)
            if dangling:
                self.assertFalse(sentinel.exists())
            else:
                self.assertEqual(sentinel.read_bytes(), b'SENTINEL' + NL.encode())
            self.assertTrue((out / plant).is_symlink())
            self.assertEqual(sorted(p.name for p in out.iterdir()), [plant])

    def test_planted_part_links_are_never_followed(self):
        for fmt in ('zip', 'tar.gz'):
            plants = ('%s-plugin-%s.%s.part' % ('irrigator', VERSION, fmt), 'release-manifest.json.part', 'SHA256SUMS.part')
            for plant in plants:
                for force in (False, True):
                    self.plant_and_build(fmt, plant, force)

    def test_dangling_part_link_is_not_created_through(self):
        for plant in ('irrigator-plugin-1.0.0.zip.part', 'SHA256SUMS.part'):
            self.plant_and_build('zip', plant, True, dangling=True)

    def test_existing_regular_part_file_is_not_overwritten(self):
        for plant in ('irrigator-plugin-1.0.0.zip.part', 'release-manifest.json.part', 'SHA256SUMS.part'):
            with tempfile.TemporaryDirectory() as d:
                root = make_tree(d)
                out = Path(d) / 'out'
                write(out, plant, 'KEEP-ME' + NL)
                rc, _o, err = run(['--root', root, '--out', out, '--force'])
                self.assertEqual(rc, 2, err)
                self.assertEqual((out / plant).read_text(), 'KEEP-ME' + NL)
                self.assertEqual([p.name for p in out.iterdir()], [plant])

    def test_failed_rebuild_leaves_previous_artifacts_unchanged(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            a = build(self, root, Path(d) / 'out')
            before = dict((p.name, p.read_bytes()) for p in a.parent.iterdir())
            sentinel = Path(d) / 'sentinel.txt'
            sentinel.write_bytes(b'S')
            link(self, sentinel, a.parent / 'SHA256SUMS.part')
            rc, _o, _e = run(['--root', root, '--out', a.parent, '--force'])
            self.assertEqual(rc, 2)
            self.assertEqual(sentinel.read_bytes(), b'S')
            after = dict((p.name, p.read_bytes()) for p in a.parent.iterdir() if not p.is_symlink())
            self.assertEqual(after, before)


class SidecarHandling(unittest.TestCase):
    def test_symlinked_sidecars_are_refused_and_not_echoed(self):
        for name in ('SHA256SUMS', 'release-manifest.json'):
            with tempfile.TemporaryDirectory() as d:
                a = build(self, make_tree(d), Path(d) / 'out')
                original = a.parent / name
                private = Path(d) / 'private.txt'
                private.write_bytes(original.read_bytes() + (MARKER + NL).encode())
                original.unlink()
                link(self, private, original)
                rc, out, err = run(['--verify', a])
                self.assertEqual(rc, 1, err)
                self.assertIn('symlink', err)
                self.assertNotIn(MARKER, out + err)

    def test_valid_content_behind_a_symlink_is_still_refused(self):
        with tempfile.TemporaryDirectory() as d:
            a = build(self, make_tree(d), Path(d) / 'out')
            original = a.parent / 'SHA256SUMS'
            copy = Path(d) / 'copy.txt'
            copy.write_bytes(original.read_bytes())
            original.unlink()
            link(self, copy, original)
            rc, _o, _e = run(['--verify', a])
            self.assertEqual(rc, 1)

    def test_sidecar_directory_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            a = build(self, make_tree(d), Path(d) / 'out')
            (a.parent / 'SHA256SUMS').unlink()
            (a.parent / 'SHA256SUMS').mkdir()
            rc, _o, _e = run(['--verify', a])
            self.assertEqual(rc, 1)

    def test_tampered_sidecar_content_is_not_echoed(self):
        with tempfile.TemporaryDirectory() as d:
            a = build(self, make_tree(d), Path(d) / 'out')
            sums, relman = a.parent / 'SHA256SUMS', a.parent / 'release-manifest.json'
            good_rel = json.loads(relman.read_text())
            sums.write_text(MARKER + ' not a digest line' + NL)
            rc, out, err = run(['--verify', a])
            self.assertEqual(rc, 1)
            self.assertNotIn(MARKER, out + err)
            bad = dict(good_rel)
            bad['archive'] = MARKER
            bad['archive_sha256'] = MARKER
            bad['files'] = list(good_rel['files']) + [{'path': MARKER, 'sha256': MARKER}]
            relman.write_text(json.dumps(bad))
            rc, out, err = run(['--verify', a])
            self.assertEqual(rc, 1)
            self.assertNotIn(MARKER, out + err)
            relman.write_text('{' + MARKER)
            rc, out, err = run(['--verify', a])
            self.assertEqual(rc, 1)
            self.assertNotIn(MARKER, out + err)

    def test_absent_sidecars_warn_unless_required(self):
        with tempfile.TemporaryDirectory() as d:
            a = build(self, make_tree(d), Path(d) / 'out')
            (a.parent / 'SHA256SUMS').unlink()
            (a.parent / 'release-manifest.json').unlink()
            rc, _o, err = run(['--verify', a])
            self.assertEqual(rc, 0, err)
            self.assertIn('warning', err)
            rc, _o, err = run(['--verify', a, '--require-sidecars'])
            self.assertEqual(rc, 1)
            self.assertIn('--require-sidecars', err)


class SecretFilenames(unittest.TestCase):
    def test_known_secret_filenames_are_rejected(self):
        for rel in ('docs/.env', 'docs/.env.production', 'docs/credentials.json', 'docs/.credentials',
                    'skills/plan/settings.local.json', 'docs/settings.local.md'):
            with tempfile.TemporaryDirectory() as d:
                root = make_tree(d)
                write(root, rel, 'harmless-body' + NL)
                rc, _o, err = run(['--root', root, '--check'])
                self.assertEqual(rc, 1, rel)
                self.assertIn('secret-looking filename', err)
                self.assertNotIn('harmless-body', err)

    def test_similar_but_legitimate_names_pass(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            write(root, 'docs/credentials-guide.md', 'Notes' + NL)
            write(root, 'docs/settings-local.md', 'Notes' + NL)
            rc, _o, err = run(['--root', root, '--check'])
            self.assertEqual(rc, 0, err)

    def test_archive_member_with_secret_filename_fails_verify(self):
        with tempfile.TemporaryDirectory() as d:
            a = build(self, make_tree(d), Path(d) / 'out')
            evil = Path(d) / 'evil' / a.name
            evil.parent.mkdir()
            write_zip(evil, zip_members(a) + [(PREFIX + '/docs/credentials.json', b'{}')])
            rc, _o, err = run(['--verify', evil])
            self.assertEqual(rc, 1)
            self.assertIn('secret', err)

    def test_heuristic_scan_catches_known_shapes_but_is_not_proof(self):
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            write(root, 'docs/key.md', 'AKIA' + 'ABCDEFGHIJKLMNOP' + NL)
            rc, _o, err = run(['--root', root, '--check'])
            self.assertEqual(rc, 1)
            self.assertNotIn('ABCDEFGHIJKLMNOP', err)
        with tempfile.TemporaryDirectory() as d:
            root = make_tree(d)
            write(root, 'docs/notes.md', 'password=hunter2' + NL)
            rc, _o, _e = run(['--root', root, '--check'])
            self.assertEqual(rc, 0)  # documents a scan limitation, not an endorsement

    @unittest.skipUnless((ROOT / 'docs' / 'local-release.md').is_file(), 'no docs in this checkout')
    def test_docs_state_scan_limitations(self):
        text = (ROOT / 'docs' / 'local-release.md').read_text(encoding='utf-8')
        self.assertIn('does not prove', text)
        self.assertIn('--require-sidecars', text)


class ManifestPathFields(unittest.TestCase):
    def check(self, extra):
        with tempfile.TemporaryDirectory() as d:
            return run(['--root', make_tree(d, extra), '--check'])

    def test_unsafe_path_fields_are_rejected(self):
        bad = ({'icon': '/etc/passwd'}, {'logo': '~/x.png'}, {'iconPath': 'C:' + chr(92) + 'x.png'},
               {'screenshots': ['assets/../x.png']}, {'file': 'file:///etc/passwd'},
               {'iconPath': '$HOME/x.png'}, {'icon_path': '~'})
        for extra in bad:
            rc, _o, err = self.check(extra)
            self.assertEqual(rc, 1, extra)
            self.assertIn('unsafe path', err)

    def test_legitimate_public_text_and_urls_pass(self):
        good = {'description': 'Use ~/notes and /usr/bin/env as plain text',
                'homepage': 'https://example.com/irrigator',
                'icon': 'https://example.com/icon.png',
                'displayName': '/ not a path',
                'profile': '/plain/text'}
        rc, _o, err = self.check(good)
        self.assertEqual(rc, 0, err)

    def test_helpers(self):
        self.assertIsNone(P.unsafe_manifest_path('https://example.com/a'))
        self.assertIsNone(P.unsafe_manifest_path('notes/readme.md'))
        for val in ('/etc/passwd', '~', '~/x', 'C:/x', 'file:///x', '$HOME/x', 'a/../b'):
            self.assertIsNotNone(P.unsafe_manifest_path(val), val)
        self.assertTrue(P.is_path_field('iconPath'))
        self.assertTrue(P.is_path_field('skills'))
        self.assertFalse(P.is_path_field('description'))
        self.assertFalse(P.is_path_field('profile'))


class MaliciousArchives(unittest.TestCase):
    def verify_crafted(self, members, fmt='zip'):
        with tempfile.TemporaryDirectory() as d:
            a = build(self, make_tree(d), Path(d) / 'out')
            evil = Path(d) / 'evil' / ('x.' + fmt)
            evil.parent.mkdir()
            if fmt == 'zip':
                write_zip(evil, zip_members(a) + members)
            else:
                write_tar(evil, zip_members(a) + members)
            return run(['--verify', evil])

    def test_collisions_fail_closed(self):
        e_acute, e_plain, combining = chr(0xe9), 'e', chr(0x301)
        cases = {
            'nfc-vs-nfd': [(PREFIX + '/docs/' + e_acute + '.md', b'a'), (PREFIX + '/docs/' + e_plain + combining + '.md', b'b')],
            'case': [(PREFIX + '/readme.md', b'x')],
            'compat': [(PREFIX + '/docs/' + chr(0xff21) + '.md', b'a'), (PREFIX + '/docs/A.md', b'b')],
            'duplicate': [(PREFIX + '/README.md', b'dup')],
        }
        for label, members in cases.items():
            for fmt in ('zip', 'tar.gz'):
                rc, out, err = self.verify_crafted(members, fmt)
                self.assertEqual(rc, 1, label + ' ' + fmt)
                self.assertIn('collision', err, label + ' ' + fmt)
                self.assertIn('FAIL', out)

    def test_file_and_directory_prefix_conflicts_fail(self):
        for members in ([(PREFIX + '/README.md/extra.md', b'x')],
                        [(PREFIX + '/docs/a', b'x'), (PREFIX + '/docs/a/b.md', b'y')]):
            rc, _o, err = self.verify_crafted(members)
            self.assertEqual(rc, 1)
            self.assertIn('conflict', err)

    def test_conflicting_top_level_prefixes_fail(self):
        rc, _o, err = self.verify_crafted([('other-plugin-1.0.0/README.md', b'x')])
        self.assertEqual(rc, 1)
        self.assertIn('top-level', err)

    def test_invisible_and_unsafe_names_fail(self):
        for name in (PREFIX + '/docs/a' + chr(0x202e) + 'b.md', PREFIX + '/../evil.md', '/abs/evil.md'):
            rc, _o, _e = self.verify_crafted([(name, b'x')])
            self.assertEqual(rc, 1, ascii(name))

    def test_collision_helpers(self):
        self.assertEqual(P.collision_key(chr(0xe9)), P.collision_key('e' + chr(0x301)))
        self.assertEqual(P.collision_key('README.md'), P.collision_key('readme.MD'))
        self.assertEqual(P.conflict_keys(['a', 'a/b', 'c']), ['a/b'])


if __name__ == '__main__':
    unittest.main(verbosity=2 if '-v' in sys.argv else 1)
