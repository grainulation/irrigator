#!/usr/bin/env python3
'''Irrigator installed-consumer check (stdlib only, local only, read-only on the bundle).

This is the CONSUMER layer. verification/plugin-contract-test.py checks a repository
checkout (or any explicit root). This script checks a package the way a consumer
receives it: one explicit extracted source-only bundle, using the validators copied
INTO that bundle, run with -I -B from a private temporary working directory with a
private HOME and TMPDIR. Nothing is installed, built, searched for, pushed or published.

Usage (the bundle is always explicit):
  python3 -I verification/plugin-consumer-test.py --root EXTRACTED_BUNDLE [--json] [--strict]
  python3 -I verification/plugin-consumer-test.py --archive RELEASE.zip|.tar.gz [--json]
  python3 -I verification/plugin-consumer-test.py --root EXTRACTED_BUNDLE --binary /abs/path/irrigator

--archive validates every member name and type, then extracts only regular files into a
private temporary directory (never into the working tree). --root is used as given and is
never written to; a before/after digest of the bundle must match.

--binary is optional, must be an absolute path to an executable that lives OUTSIDE the
bundle (the archive is source-only and bundles no runtime). PATH is never searched and
nothing is built. With it, the bundle's own contract test runs its credential-free
mock-backend smoke. Without it the smoke is reported as SKIP.

What a pass does NOT show: that Codex or Claude Code discovers or loads the plugin. The
skill checks here are file checks. Native host discovery is untested by this script.

Exit status: 0 = no FAIL (and no WARN with --strict), 1 = FAIL, 2 = usage error.
'''
import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tarfile
import tempfile
import zipfile
import zlib
from pathlib import Path

sys.dont_write_bytecode = True

LAYER = 'installed-consumer'
SKILLS = ('irrigator', 'router', 'plan', 'run', 'resume', 'observe', 'debug', 'budget', 'swarm')
SCOPE = ('installed-consumer static and mock-CLI checks of one explicit extracted bundle; the plugin is not '
         'loaded by Codex or Claude Code, so native host discovery is not exercised (untested)')
LAYOUT = ('plugin.json', '.codex-plugin/plugin.json', '.claude-plugin/plugin.json', 'README.md',
          'docs/local-release.md', 'tools/package-plugin.py', 'verification/plugin-contract-test.py',
          'verification/cli-contract.json', 'docs/plan.schema.json')
CHECKOUT_MARKERS = ('.git', 'go.mod', 'main.go', 'Makefile')
TEXT_SUFFIXES = ('.md', '.json', '.py', '.txt', '.sh', '.toml', '.yaml', '.yml')
MAX_MEMBERS = 500
MAX_FILE_BYTES = 5 * 1024 * 1024
MAX_TOTAL_BYTES = 25 * 1024 * 1024
NATIVE_MAGIC = tuple(bytes.fromhex(h) for h in ('7f454c46', 'cffaedfe', 'cefaedfe', 'cafebabe', 'feedfacf', 'feedface'))
LOCAL_PATH = re.compile(r'/(?:Users|home)/[A-Za-z0-9._-]+/')
NL = chr(10)


class Reporter:
    def __init__(self):
        self.rows = []

    def add(self, level, tid, msg):
        self.rows.append({'level': level, 'id': tid, 'message': msg})

    def check(self, tid, ok, msg, level='FAIL'):
        self.add('PASS' if ok else level, tid, msg)
        return ok

    def count(self, level):
        return sum(1 for r in self.rows if r['level'] == level)


def member_problem(name):
    '''Reason an archive member name is unsafe, or None.'''
    if not name:
        return 'empty path'
    if name.startswith('/') or chr(92) in name:
        return 'absolute path or backslash'
    if re.match(r'^[A-Za-z]:', name):
        return 'drive-letter path'
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in name):
        return 'control character'
    if any(part in ('', '.', '..') for part in name.split('/')):
        return 'empty, dot or dot-dot component'
    return None


def read_members(path):
    '''Return [(name, bytes)] for regular members. Raise ValueError on anything unsafe.'''
    members, total = [], 0
    path = str(path)
    if zipfile.is_zipfile(path):
        with zipfile.ZipFile(path) as zf:
            infos = zf.infolist()
            if len(infos) > MAX_MEMBERS:
                raise ValueError('too many members: %d' % len(infos))
            for info in infos:
                if info.is_dir():
                    continue
                mode = (info.external_attr >> 16) & 0o170000
                if mode not in (0, 0o100000):
                    raise ValueError('non-regular zip member: %r' % info.filename)
                if info.flag_bits & 1:
                    raise ValueError('encrypted member: %r' % info.filename)
                if info.file_size > MAX_FILE_BYTES:
                    raise ValueError('member too large: %r' % info.filename)
                with zf.open(info) as fh:
                    data = fh.read(MAX_FILE_BYTES + 1)
                if len(data) != info.file_size:
                    raise ValueError('member size does not match its header: %r' % info.filename)
                total += len(data)
                if total > MAX_TOTAL_BYTES:
                    raise ValueError('archive expands beyond %d bytes' % MAX_TOTAL_BYTES)
                members.append((info.filename, data))
    elif tarfile.is_tarfile(path):
        with tarfile.open(path, 'r:*') as tf:
            count = 0
            for m in tf:
                count += 1
                if count > MAX_MEMBERS:
                    raise ValueError('too many members (more than %d)' % MAX_MEMBERS)
                if m.isdir():
                    continue
                if not m.isreg():
                    raise ValueError('non-regular tar member: %r' % m.name)
                if m.size > MAX_FILE_BYTES:
                    raise ValueError('member too large: %r' % m.name)
                fh = tf.extractfile(m)
                data = fh.read(MAX_FILE_BYTES + 1) if fh is not None else b''
                if len(data) != m.size:
                    raise ValueError('member size does not match its header: %r' % m.name)
                total += len(data)
                if total > MAX_TOTAL_BYTES:
                    raise ValueError('archive expands beyond %d bytes' % MAX_TOTAL_BYTES)
                members.append((m.name, data))
    else:
        raise ValueError('not a zip or tar archive: %s' % path)
    return members


def extract_archive(archive, dest):
    '''Validate every member, then write regular files under dest. Return the top-level directory.'''
    try:
        members = read_members(archive)
    except (OSError, EOFError, zlib.error, zipfile.BadZipFile, tarfile.TarError) as exc:
        raise ValueError('cannot read archive: %s' % exc)
    if not members:
        raise ValueError('archive has no regular files')
    dest = Path(dest).resolve()
    prefixes, seen = set(), {}
    for name, _data in members:
        problem = member_problem(name)
        if problem:
            raise ValueError('unsafe member name %r: %s' % (name, problem))
        parts = name.split('/')
        if len(parts) < 2:
            raise ValueError('member outside the single top-level directory: %r' % name)
        prefixes.add(parts[0])
        key = name.casefold()
        if key in seen:
            raise ValueError('duplicate or case-colliding member: %r' % name)
        seen[key] = name
    if len(prefixes) != 1:
        raise ValueError('archive must have exactly one top-level directory, found %s' % sorted(prefixes))
    for name, data in members:
        target = (dest / name).resolve()
        if dest not in target.parents:
            raise ValueError('member escapes the extraction directory: %r' % name)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    return dest / next(iter(prefixes))


def sha256_file(path):
    h = hashlib.sha256()
    with open(str(path), 'rb') as fh:
        for chunk in iter(lambda: fh.read(65536), b''):
            h.update(chunk)
    return h.hexdigest()


def digest_tree(root):
    '''Map of relative path -> content digest (files, symlink targets, directories).'''
    root = Path(root)
    out = {}
    for dirpath, dirnames, filenames in os.walk(str(root), followlinks=False):
        dirnames.sort()
        for n in dirnames:
            out[(Path(dirpath) / n).relative_to(root).as_posix() + '/'] = 'dir'
        for n in sorted(filenames):
            p = Path(dirpath) / n
            rel = p.relative_to(root).as_posix()
            if p.is_symlink():
                out[rel] = 'symlink:' + os.readlink(str(p))
            else:
                try:
                    out[rel] = sha256_file(p)
                except OSError as exc:
                    out[rel] = 'unreadable:' + exc.__class__.__name__
    return out


def inside_root(root, path):
    root, path = Path(root).resolve(), Path(path).resolve()
    return path == root or root in path.parents


def private_env(work):
    home, tmp = work / 'home', work / 'tmp'
    home.mkdir()
    tmp.mkdir()
    return {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': str(home), 'TMPDIR': str(tmp),
            'PYTHONDONTWRITEBYTECODE': '1', 'IRRIGATOR_UI': 'plain', 'NO_COLOR': '1'}


def run(cmd, cwd, env, timeout):
    try:
        p = subprocess.run(cmd, cwd=str(cwd), env=env, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError) as exc:
        return None, '', str(exc)
    return p.returncode, p.stdout, p.stderr


def frontmatter_name(text):
    lines = text.splitlines()
    if not lines or lines[0].strip() != '---':
        return None
    for line in lines[1:]:
        if line.strip() == '---':
            return None
        m = re.match(r'^name:[ ]*(.*)$', line)
        if m:
            return m.group(1).strip().strip(chr(34) + chr(39))
    return None


def iter_files(root):
    for dirpath, dirnames, filenames in os.walk(str(root), followlinks=False):
        dirnames.sort()
        for n in sorted(filenames):
            p = Path(dirpath) / n
            yield p, p.relative_to(root).as_posix()


# ---------------------------------------------------------------- bundle checks
def check_checkout(rep, root):
    found = [m for m in CHECKOUT_MARKERS if (root / m).exists()]
    if found:
        rep.add('WARN', 'bundle.checkout-markers',
                'root has repository markers (%s): this looks like a checkout, not an extracted bundle; use '
                'plugin-contract-test.py for repository checks' % ', '.join(found))
    else:
        rep.add('PASS', 'bundle.checkout-markers', 'root has no repository markers')


def check_layout(rep, root):
    for rel in LAYOUT:
        rep.check('bundle.layout', (root / rel).is_file(), 'required file ' + rel)


def check_skills(rep, root):
    for s in SKILLS:
        p = root / 'skills' / s / 'SKILL.md'
        if not rep.check('bundle.skill.%s' % s, p.is_file(),
                         'skills/%s/SKILL.md is present (file presence only; not host discovery)' % s):
            continue
        name = frontmatter_name(p.read_text(encoding='utf-8', errors='replace'))
        rep.check('bundle.skill.%s.name' % s, name == s, 'frontmatter name %r equals directory %r' % (name, s))
    sdir = root / 'skills'
    extra = sorted(x.name for x in sdir.iterdir() if x.is_dir()) if sdir.is_dir() else []
    unexpected = sorted(set(extra) - set(SKILLS))
    rep.check('bundle.skill-set', not unexpected, 'unexpected skill directories: %s' % unexpected, 'WARN')


def check_hygiene(rep, root):
    natives, symlinks, local_paths, licenses = [], [], [], []
    for p, rel in iter_files(root):
        name = rel.rsplit('/', 1)[-1]
        if p.is_symlink():
            symlinks.append(rel)
            continue
        try:
            with open(str(p), 'rb') as fh:
                head = fh.read(4)
        except OSError:
            head = b''
        if head[:4] in NATIVE_MAGIC or (head[:2] == b'MZ' and name.lower().endswith(('.exe', '.dll'))):
            natives.append(rel)
        if name.upper().split('.')[0] in ('LICENSE', 'LICENCE', 'COPYING'):
            licenses.append(rel)
        if name.lower().endswith(TEXT_SUFFIXES):
            try:
                if p.stat().st_size <= MAX_FILE_BYTES and LOCAL_PATH.search(p.read_text(encoding='utf-8', errors='replace')):
                    local_paths.append(rel)
            except OSError:
                pass
    rep.check('bundle.no-native', not natives,
              'no native executable in the bundle (the archive is source-only)' if not natives
              else 'native executable bundled: %s' % natives)
    rep.check('bundle.symlinks', not symlinks,
              'no symlinks in the bundle' if not symlinks else 'symlinks present: %s' % symlinks)
    rep.check('bundle.local-paths', not local_paths,
              'no machine-specific home paths in text files (placeholders are used)' if not local_paths
              else 'machine-specific home path in: %s; replace with a placeholder' % local_paths, 'WARN')
    license_path = root / 'LICENSE'
    notice = license_path.read_text(encoding='utf-8') if license_path.is_file() else ''
    rep.check('bundle.license', all(x in notice for x in ('MIT License', 'Permission is hereby granted, free of charge', 'THE SOFTWARE IS PROVIDED')),
              'bundle includes the project MIT license notice')


def check_binary(rep, root, binary):
    '''Return the binary path when it is acceptable for the smoke, else None.'''
    if not (binary.is_file() and os.access(str(binary), os.X_OK)):
        rep.add('FAIL', 'consumer.binary', 'not an executable file: %s' % binary)
        return None
    if inside_root(root, binary):
        rep.add('FAIL', 'consumer.binary-separate',
                'the binary is inside the bundle; the source-only archive must not carry the runtime, so supply it separately')
        return None
    rep.add('PASS', 'consumer.binary',
            'explicit binary outside the bundle, sha256 %s (recorded; not compared with any release)' % sha256_file(binary))
    return binary


def summarize(rep, tid, rows, label):
    if not rows:
        rep.add('FAIL', tid, '%s produced no results' % label)
        return
    fails = [r for r in rows if r['level'] == 'FAIL']
    warns = [r for r in rows if r['level'] == 'WARN']
    passes = sum(1 for r in rows if r['level'] == 'PASS')
    if fails:
        detail = '; '.join('%s %s' % (r['id'], r['message'][:100]) for r in fails[:5])
        rep.add('FAIL', tid, '%s: %d failed (first: %s)' % (label, len(fails), detail))
    else:
        rep.add('PASS', tid, '%s: %d passed, no failures' % (label, passes))
    if warns:
        ids = sorted(set(r['id'] for r in warns))
        rep.add('WARN', tid + '-warnings', '%s: %d warnings (%s)' % (label, len(warns), ', '.join(ids)))


def is_runtime_row(row):
    return row['id'].startswith(('binary.', 'smoke.', 't_binary'))


def run_contract(rep, root, archive, binary, cwd, env, timeout):
    if not binary:
        rep.add('SKIP', 'consumer.smoke', 'no usable --binary supplied; the mock CLI smoke was not run (nothing is built or searched for)')
    script = root / 'verification' / 'plugin-contract-test.py'
    if not script.is_file():
        rep.add('FAIL', 'consumer.contract-static', 'bundle lacks verification/plugin-contract-test.py')
        if binary:
            rep.add('FAIL', 'consumer.smoke', 'not run: the bundle has no contract test to drive it')
        return
    cmd = [sys.executable, '-I', '-B', str(script), '--root', str(root), '--json']
    if archive is not None:
        cmd += ['--archive', str(archive)]
    if binary:
        cmd += ['--binary', str(binary), '--smoke']
    rc, out, err = run(cmd, cwd, env, timeout)
    try:
        data = json.loads(out)
    except ValueError:
        data = None
    if not isinstance(data, dict) or not isinstance(data.get('results'), list):
        rep.add('FAIL', 'consumer.contract-static', 'bundle contract test printed no JSON report (rc=%s): %s' % (rc, err.strip()[:200]))
        if binary:
            rep.add('FAIL', 'consumer.smoke', 'not run: the contract test produced no report')
        return
    rows = data['results']
    summarize(rep, 'consumer.contract-static', [r for r in rows if not is_runtime_row(r)], 'bundle static contract checks')
    if binary:
        summarize(rep, 'consumer.smoke', [r for r in rows if is_runtime_row(r)], 'mock-backend CLI smoke')


def run_regression(rep, root, cwd, env, timeout):
    script = root / 'verification' / 'plugin-validator-regression.py'
    if not script.is_file():
        rep.add('FAIL', 'consumer.validator-regression',
                'bundle lacks verification/plugin-validator-regression.py; the packager allowlist must include it for a bundle to check itself')
        return
    rc, out, err = run([sys.executable, '-I', '-B', str(script)], cwd, env, timeout)
    tail = (err.strip().splitlines() or [''])[-1]
    rep.check('consumer.validator-regression', rc == 0, 'bundled validator regression tests (rc=%s): %s' % (rc, tail[:160]))
    if not (root / 'verification' / 'plugin-consumer-test.py').is_file():
        rep.add('SKIP', 'consumer.self-copy', 'bundle has no copy of plugin-consumer-test.py; run it from the repository against this bundle')


def run_package_tool(rep, root, archive, cwd, env, timeout):
    tool = root / 'tools' / 'package-plugin.py'
    if not tool.is_file():
        rep.add('FAIL', 'consumer.package-tool', 'bundle lacks tools/package-plugin.py')
        return
    if archive is not None:
        cmd = [sys.executable, '-I', '-B', str(tool), '--verify', str(archive)]
        label = 'package-plugin.py --verify on the archive'
    else:
        cmd = [sys.executable, '-I', '-B', str(tool), '--check', '--root', str(root)]
        label = 'package-plugin.py --check on the bundle root'
    rc, out, err = run(cmd, cwd, env, timeout)
    last = (out.strip().splitlines() or [''])[-1]
    rep.check('consumer.package-tool', rc == 0, '%s (rc=%s): %s' % (label, rc, last[:120]))
    prefix = 'warning: '
    warns = [x[len(prefix):] for x in err.splitlines() if x.startswith(prefix)]
    if warns:
        rep.add('WARN', 'consumer.package-warnings', '%d packager warnings: %s' % (len(warns), '; '.join(warns[:5])))


def run_checks(rep, root, archive, binary, cwd, env, timeout):
    before = digest_tree(root)
    check_checkout(rep, root)
    check_layout(rep, root)
    check_skills(rep, root)
    check_hygiene(rep, root)
    use_binary = check_binary(rep, root, binary) if binary else None
    run_contract(rep, root, archive, use_binary, cwd, env, timeout)
    run_regression(rep, root, cwd, env, timeout)
    run_package_tool(rep, root, archive, cwd, env, timeout)
    after = digest_tree(root)
    changed = sorted(k for k in set(before) | set(after) if before.get(k) != after.get(k))
    rep.check('consumer.bundle-unchanged', not changed,
              'checks left the bundle byte-for-byte unchanged' if not changed else 'checks modified the bundle: %s' % changed[:10])


def finish(rep, args, label):
    counts = dict((k, rep.count(k)) for k in ('PASS', 'WARN', 'FAIL', 'SKIP'))
    if args.json:
        print(json.dumps({'layer': LAYER, 'scope': SCOPE, 'target': label, 'summary': counts, 'results': rep.rows}, indent=2))
    else:
        for row in rep.rows:
            if row['level'] != 'PASS':
                print('[%s] %s: %s' % (row['level'], row['id'], row['message']))
        print(NL + 'layer: ' + LAYER)
        print('scope: ' + SCOPE)
        print('summary: %d pass, %d warn, %d fail, %d skip (%s)' % (
            counts['PASS'], counts['WARN'], counts['FAIL'], counts['SKIP'], label))
    if counts['FAIL'] or (args.strict and counts['WARN']):
        return 1
    return 0


def main(argv=None):
    ap = argparse.ArgumentParser(description='Irrigator installed-consumer check (local, read-only on the bundle)')
    grp = ap.add_mutually_exclusive_group(required=True)
    grp.add_argument('--root', help='explicit extracted bundle directory (never written to)')
    grp.add_argument('--archive', help='release .zip/.tar.gz; validated and extracted into a private temporary directory')
    ap.add_argument('--binary', help='absolute path to a separately supplied irrigator executable outside the bundle (enables the mock smoke)')
    ap.add_argument('--json', action='store_true', help='machine-readable report')
    ap.add_argument('--strict', action='store_true', help='WARN also fails')
    ap.add_argument('--timeout', type=int, default=300, help='seconds allowed per child process')
    args = ap.parse_args(argv)
    binary = None
    if args.binary:
        binary = Path(args.binary)
        if not binary.is_absolute():
            print('--binary must be an absolute path; PATH is never searched', file=sys.stderr)
            return 2
    archive = root = None
    if args.archive:
        archive = Path(args.archive).resolve()
        if not archive.is_file():
            print('archive is not a file: %s' % archive, file=sys.stderr)
            return 2
    else:
        root = Path(args.root).resolve()
        if not root.is_dir():
            print('root is not a directory: %s' % root, file=sys.stderr)
            return 2
    rep = Reporter()
    label = ('archive %s' % archive) if archive is not None else ('root %s' % root)
    with tempfile.TemporaryDirectory(prefix='irrigator-consumer-') as w:
        work = Path(w)
        env = private_env(work)
        cwd = work / 'cwd'
        cwd.mkdir()
        if archive is not None:
            ex = work / 'extract'
            ex.mkdir()
            try:
                root = extract_archive(archive, ex)
            except ValueError as exc:
                rep.add('FAIL', 'consumer.extract', 'archive refused: %s' % exc)
                return finish(rep, args, label)
            rep.add('PASS', 'consumer.extract', 'validated members and extracted regular files into a private temporary directory')
        try:
            run_checks(rep, root, archive, binary, cwd, env, args.timeout)
        except Exception as exc:  # noqa: BLE001 - a crashing check must fail, never pass
            rep.add('FAIL', 'consumer.crashed', 'check crashed: %r' % (exc,))
        return finish(rep, args, label)


if __name__ == '__main__':
    sys.exit(main())
