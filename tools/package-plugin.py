#!/usr/bin/env python3
'''Local-only packager for the Irrigator plugin/skill bundle.

What it does
  * validates the plugin manifests and skills that exist in the working tree
  * builds a deterministic archive (zip or tar.gz) plus SHA256SUMS and
    release-manifest.json inside an output directory you choose
  * audits an existing archive without extracting it into your tree (--verify)

What it never does
  * no network access, no git operations, no push/publish, no host install
  * does not build or bundle the Go runtime binary (build it with `make build`);
    --binary only records that binary's SHA-256 and size, never opens it for execution
  * does not create or choose a license; a LICENSE file is only included if one
    already exists in the tree

Usage (run from anywhere; the tree is passed as an argument):
  python3 -I tools/package-plugin.py --check
  python3 -I tools/package-plugin.py --list
  python3 -I tools/package-plugin.py --out /tmp/irrigator-dist
  python3 -I tools/package-plugin.py --root /path/to/irrigator --format tar.gz --force
  python3 -I tools/package-plugin.py --out /tmp/irrigator-dist --binary /absolute/path/to/irrigator
  python3 -I tools/package-plugin.py --verify /tmp/irrigator-dist/ARCHIVE
  python3 -I tools/package-plugin.py --verify /tmp/irrigator-dist/ARCHIVE --require-sidecars

Exit codes: 0 ok, 1 validation failed, 2 usage or I/O error.
'''
import argparse
import contextlib
import gzip
import hashlib
import io
import json
import os
import re
import stat
import sys
import tarfile
import tempfile
import time
import unicodedata
import zipfile
import zlib
from pathlib import Path

NL = chr(10)
BACKSLASH = chr(92)

PLUGIN_NAME = 'irrigator'
REQUIRED_SKILLS = ('irrigator', 'router', 'plan', 'run', 'resume', 'observe', 'budget', 'swarm', 'debug')
ROOT_MANIFEST = 'plugin.json'
LEGACY_CODEX_MANIFEST = '.codex-plugin/plugin.json'
MANIFESTS = (LEGACY_CODEX_MANIFEST, ROOT_MANIFEST, '.claude-plugin/plugin.json')
MARKETPLACE = '.claude-plugin/marketplace.json'
MAX_FILE_BYTES = 5 * 1024 * 1024
MAX_TOTAL_BYTES = 25 * 1024 * 1024
MAX_FILES = 500
ZIP_MIN_EPOCH = 315532800   # 1980-01-01, the zip minimum
ZIP_MAX_EPOCH = 4354819199  # 2107-12-31, the zip maximum
TEXT_EXTENSIONS = {'.md', '.json', '.py', '.txt', '.sh', '.toml', '.yaml', '.yml'}

# Allowlist: only these tree entries are packaged. Anything else is ignored.
INCLUDE_DIRS = ('.claude-plugin', '.codex-plugin', 'skills', 'docs')
INCLUDE_FILES = (
    'plugin.json',
    'README.md',
    'CONTRIBUTING.md',
    'CHANGELOG.md',
    'CODE_OF_CONDUCT.md',
    'SECURITY.md',
    'verification/plugin-contract-test.py',
    'verification/cli-contract.json',
    'verification/plugin-validator-regression.py',
    'verification/plugin-consumer-test.py',
    'tools/package-plugin.py',
    'tools/test_parity_package.py',
    'tools/host-plugin-probe.py',
    'tools/host-agent-events.py',
    'verification/host-agent-events-test.py',
)
INCLUDE_GLOBS = ('LICENSE*', 'NOTICE*')  # only if already present; never generated
EXCLUDE_DIR_NAMES = {'.git', 'dist', '__pycache__', 'node_modules', 'reviews', '.venv'}
# docs/mascot.png is only the README image; it is not part of the plugin.
EXCLUDE_FILE_NAMES = {'.DS_Store', 'mascot.png'}
# Known secret-bearing filenames are refused outright, never silently skipped.
SECRET_FILENAME = re.compile(r'[.]env.*|[.]?credentials?(?:[.].*)?|[.]?settings[.]local(?:[.].*)?', re.IGNORECASE)
# Manifest keys whose last word names a file-system location. Only these string
# values are checked for absolute, tilde or similar unsafe paths; descriptions,
# prompts and public URLs are left alone.
PATH_FIELD_WORDS = {'path', 'file', 'dir', 'directory', 'folder', 'icon', 'logo', 'image',
                    'screenshot', 'skill', 'agent', 'hook', 'entry', 'source', 'binary', 'executable'}

# Irrigator has no MCP server and the plugin makes no permission or pricing claims.
FORBIDDEN_MANIFEST_KEYS = {'mcpServers', 'mcp_servers', 'permissions', 'pricing', 'price', 'userConfig'}
# Present only with owner authorization; warn so they are never added by accident.
REMOTE_METADATA_KEYS = {'homepage', 'repository', 'websiteURL', 'privacyPolicyURL', 'termsOfServiceURL'}

BINARY_MAGIC = tuple(bytes.fromhex(h) for h in ('7f454c46', 'cffaedfe', 'cefaedfe', 'cafebabe', 'feedfacf', 'feedface'))
# Coarse heuristics only: a clean scan does not prove a package holds no secrets.
# Hits are reported by file and kind, never by matched text.
SECRET_PATTERNS = (
    ('private key block', re.compile(r'-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----')),
    ('AWS access key id', re.compile(r'(?<![A-Za-z0-9])AKIA[0-9A-Z]{16}(?![A-Za-z0-9])')),
    ('GitHub token', re.compile(r'(?<![A-Za-z0-9])gh[pousr]_[A-Za-z0-9]{36,}')),
    ('API secret key', re.compile(r'(?<![A-Za-z0-9])sk-[A-Za-z0-9_-]{24,}')),
    ('Slack token', re.compile(r'(?<![A-Za-z0-9])xox[abprs]-[A-Za-z0-9-]{10,}')),
)
LOCAL_PATH = re.compile(r'/(?:Users|home)/[A-Za-z0-9._-]+/')
SEMVER = re.compile(r'[0-9]+[.][0-9]+[.][0-9]+(?:[-+][0-9A-Za-z.-]+)?')
VERSION_LABEL = re.compile(r'[0-9A-Za-z][0-9A-Za-z.+-]*')


class Report:
    def __init__(self):
        self.errors = []
        self.warnings = []

    def error(self, msg):
        self.errors.append(msg)

    def warn(self, msg):
        self.warnings.append(msg)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as fh:
        for chunk in iter(lambda: fh.read(65536), b''):
            h.update(chunk)
    return h.hexdigest()


def path_problem(name):
    '''Return a reason a relative archive/tree path is unsafe, or None.'''
    if not name:
        return 'empty path'
    if name.startswith('/') or BACKSLASH in name:
        return 'absolute path or backslash'
    if re.match(r'^[A-Za-z]:', name):
        return 'drive-letter path'
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in name):
        return 'control character in path'
    if any(unicodedata.category(ch) in ('Cc', 'Cf', 'Cs', 'Zl', 'Zp') for ch in name):
        return 'control, invisible formatting or unpaired surrogate character in path'
    if any(part in ('', '.', '..') for part in name.split('/')):
        return 'empty, dot or dot-dot path component'
    return None


def shown(text, limit=80):
    '''Short ASCII-safe rendering of an untrusted name for messages.'''
    text = str(text)
    return ascii(text[:limit]) + ('...' if len(text) > limit else '')


def collision_key(rel):
    '''Key under which paths collide on case-insensitive or normalizing file systems.'''
    return unicodedata.normalize('NFKC', unicodedata.normalize('NFKC', rel).casefold())


def conflict_keys(keys):
    '''Keys that are both a file and a directory prefix of another key.'''
    keyset = set(keys)
    bad = []
    for key in sorted(keyset):
        parts = key.split('/')
        if any('/'.join(parts[:i]) in keyset for i in range(1, len(parts))):
            bad.append(key)
    return bad


def secret_filename(name):
    return SECRET_FILENAME.fullmatch(name) is not None


def link_problem(root, rel):
    '''Return the first symlinked (or uninspectable) component of rel below root, or None.'''
    cur = str(root)
    done = []
    for part in rel.split('/'):
        if part in ('', '.'):
            continue
        cur = os.path.join(cur, part)
        done.append(part)
        try:
            mode = os.lstat(cur).st_mode
        except FileNotFoundError:
            return None
        except OSError:
            return '/'.join(done)
        if stat.S_ISLNK(mode):
            return '/'.join(done)
    return None


def read_regular(root, rel):
    '''Read one packaged file without following links; at most MAX_FILE_BYTES + 1 bytes.'''
    bad = link_problem(root, rel)
    if bad:
        raise OSError('symlink in path: %s' % bad)
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_NONBLOCK', 0) | getattr(os, 'O_CLOEXEC', 0)
    with os.fdopen(os.open(os.path.join(str(root), *rel.split('/')), flags), 'rb') as fh:
        if not stat.S_ISREG(os.fstat(fh.fileno()).st_mode):
            raise OSError('not a regular file')
        return fh.read(MAX_FILE_BYTES + 1)


def is_path_field(key):
    words = re.findall(r'[A-Z]?[a-z0-9]+|[A-Z]+(?![a-z])', str(key))
    if not words:
        return False
    last = words[-1].lower()
    return last in PATH_FIELD_WORDS or (last.endswith('s') and last[:-1] in PATH_FIELD_WORDS)


def unsafe_manifest_path(val):
    '''Reason a manifest path-field value is unsafe, or None. Public URLs are fine.'''
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in val):
        return 'control character'
    if re.match(r'^[A-Za-z]:', val) and (len(val) == 2 or val[2] in ('/', BACKSLASH)):
        return 'drive-letter path'
    low = val.lower()
    if low.startswith('file:'):
        return 'file URL'
    if re.match(r'^[a-z][a-z0-9+.-]*://', low):
        return None
    if val.startswith('~'):
        return 'tilde home path'
    if val.startswith('/'):
        return 'absolute path'
    if val.startswith('$') or re.match(r'^%[A-Za-z_]+%', val):
        return 'environment-variable path'
    if BACKSLASH in val:
        return 'backslash in path'
    if '..' in val.split('/'):
        return 'parent-directory component'
    return None


def walk_strings(obj, key='', path=''):
    '''Yield (path, nearest dict key, value) for every string in a JSON value.'''
    if isinstance(obj, dict):
        for k, v in obj.items():
            yield from walk_strings(v, k, '%s/%s' % (path, k))
    elif isinstance(obj, list):
        for idx, v in enumerate(obj):
            yield from walk_strings(v, key, '%s[%d]' % (path, idx))
    elif isinstance(obj, str):
        yield path, key, obj


def allowed_rel(rel):
    '''True if a path relative to the plugin root belongs to the packaging allowlist.'''
    parts = rel.split('/')
    name = parts[-1]
    if any(p in EXCLUDE_DIR_NAMES for p in parts[:-1]):
        return False
    if name in EXCLUDE_FILE_NAMES or secret_filename(name):
        return False
    if rel in INCLUDE_FILES:
        return True
    if len(parts) == 1:
        return name.startswith(('LICENSE', 'NOTICE'))
    if parts[0] not in INCLUDE_DIRS:
        return False
    return Path(name).suffix.lower() in TEXT_EXTENSIONS


def load_json(root, rel, rep, required=True):
    path = root / rel
    bad = link_problem(root, rel)
    if bad:
        rep.error('%s: symlink in path refused: %s' % (rel, bad))
        return None
    if not path.is_file():
        if required:
            rep.error('missing required file: %s' % rel)
        return None
    try:
        data = json.loads(path.read_text(encoding='utf-8'))
    except (OSError, ValueError) as exc:
        rep.error('%s: invalid JSON (%s)' % (rel, exc))
        return None
    if not isinstance(data, dict):
        rep.error('%s: top-level value must be an object' % rel)
        return None
    return data


def parse_frontmatter(text):
    lines = text.splitlines()
    if not lines or lines[0].strip() != '---':
        return None
    meta = {}
    for line in lines[1:]:
        if line.strip() == '---':
            return meta
        m = re.match(r'^([A-Za-z][A-Za-z0-9_-]*):[ ]*(.*)$', line)
        if m:
            meta[m.group(1)] = m.group(2).strip().strip('\'"')
    return None  # unterminated


def walk(obj, path=''):
    if isinstance(obj, dict):
        for key, val in obj.items():
            here = '%s/%s' % (path, key)
            yield here, key, val
            yield from walk(val, here)
    elif isinstance(obj, list):
        for idx, val in enumerate(obj):
            yield from walk(val, '%s[%d]' % (path, idx))


def openai_extension(data):
    '''Return the extensions.com.openai object of a portable manifest, or None.'''
    ext = data.get('extensions')
    if not isinstance(ext, dict):
        return None
    node = ext.get('com.openai')
    if isinstance(node, dict):
        return node
    com = ext.get('com')
    if isinstance(com, dict) and isinstance(com.get('openai'), dict):
        return com['openai']
    return None


def declares_local_executor(rel, data):
    '''Per the staged official notes: root plugin.json uses
    extensions.com.openai.requires_local_executor; the legacy manifest uses a
    top-level requires_local_executor. None means not applicable.'''
    if rel == ROOT_MANIFEST:
        node = openai_extension(data)
        return bool(node) and node.get('requires_local_executor') is True
    if rel == LEGACY_CODEX_MANIFEST:
        return data.get('requires_local_executor') is True
    return None


def mcp_backed(root):
    '''MCP claims are allowed only while docs/mcp.md and the `mcp` CLI command exist.'''
    try:
        contract = json.loads((Path(root) / 'verification/cli-contract.json').read_text(encoding='utf-8'))
    except (OSError, ValueError):
        return False
    names = {c.get('name') for c in contract.get('commands', []) if isinstance(c, dict)}
    return 'mcp' in names and (Path(root) / 'docs/mcp.md').is_file()


def mcp_server_entry_ok(entry):
    '''The shipped plugin may register only the read-only `irrigator mcp` server.'''
    if not isinstance(entry, dict) or set(entry) - {'command', 'args'}:
        return False
    args = entry.get('args')
    return (entry.get('command') == 'irrigator' and isinstance(args, list)
            and args[:1] == ['mcp'] and all(isinstance(a, str) for a in args)
            and '--allow-control' not in args)


def check_manifest_claims(root, rel, data, rep, has_license):
    backed_mcp = rel == '.claude-plugin/plugin.json' and mcp_backed(root)
    for here, key, val in walk(data):
        if key == 'mcpServers' and here == '/mcpServers' and backed_mcp:
            servers = val if isinstance(val, dict) else {}
            if set(servers) != {'irrigator'} or not mcp_server_entry_ok(servers['irrigator']):
                rep.error('%s: mcpServers may only register the read-only irrigator mcp server (no --allow-control)' % rel)
            continue
        if here.startswith('/mcpServers/') and backed_mcp:
            continue
        if key in FORBIDDEN_MANIFEST_KEYS:
            rep.error('%s: key %s is forbidden (no MCP server, permission or pricing claims)' % (rel, here))
        if key in REMOTE_METADATA_KEYS:
            rep.warn('%s: %s is set; add homepage/repository style fields only with owner authorization' % (rel, here))
        if isinstance(val, str) and (val.startswith('./') or val.startswith('../')):
            target = '/'.join(b for b in val.split('/') if b not in ('', '.'))
            if '..' in val.split('/'):
                rep.error('%s: %s escapes the plugin root: %s' % (rel, here, val))
            elif any(ord(ch) < 32 or ord(ch) == 127 for ch in val):
                rep.error('%s: %s contains a control character' % (rel, here))
            elif target and link_problem(root, target):
                rep.error('%s: %s resolves through a symlink: %s' % (rel, here, val))
            elif not (root / val).exists():
                rep.error('%s: %s references missing path %s (resolved from the plugin root)' % (rel, here, val))
    for here, key, val in walk_strings(data):
        if is_path_field(key) and not val.startswith(('./', '../')):
            why = unsafe_manifest_path(val)
            if why:
                rep.error('%s: %s is an unsafe path field (%s): %s' % (rel, here, why, shown(val, 60)))
    for lic in ('license', 'licence'):
        if lic in data and not has_license:
            rep.error('%s: declares a license but no LICENSE file exists; no license is chosen without authorization' % rel)


def validate_manifests(root, rep):
    '''Validate manifests. Returns (consistent version or None, host info dict).'''
    versions = {}
    hosts = {}
    found = 0
    has_license = any(root.glob('LICENSE*'))
    for rel in MANIFESTS:
        data = load_json(root, rel, rep, required=False)
        if data is None:
            if not (root / rel).exists():
                rep.warn('manifest not present: %s' % rel)
            continue
        found += 1
        if data.get('name') != PLUGIN_NAME:
            rep.error('%s: name must be %r, got %r' % (rel, PLUGIN_NAME, data.get('name')))
        ver = data.get('version')
        if not isinstance(ver, str) or not SEMVER.fullmatch(ver):
            rep.error('%s: version must be a semver string, got %r' % (rel, ver))
        else:
            versions[rel] = ver
        check_manifest_claims(root, rel, data, rep, has_license)
        skills = data.get('skills')
        if skills is not None and rel != '.claude-plugin/plugin.json' and skills not in ('./skills/', './skills'):
            rep.warn('%s: skills is %r; expected ./skills/' % (rel, skills))
        declared = declares_local_executor(rel, data)
        if declared is not None:
            hosts[rel] = {'requires_local_executor': declared}
            if not declared:
                if rel == ROOT_MANIFEST:
                    where = 'extensions.com.openai.requires_local_executor'
                else:
                    where = 'top-level requires_local_executor'
                rep.warn('%s: does not declare %s: true although Irrigator needs the local irrigator executable' % (rel, where))
    if found == 0:
        rep.error('no plugin manifest found (.codex-plugin/plugin.json, plugin.json or .claude-plugin/plugin.json)')
    if len(set(versions.values())) > 1:
        rep.error('manifest versions disagree: %s' % versions)
    root_file = root / ROOT_MANIFEST
    legacy_file = root / LEGACY_CODEX_MANIFEST
    try:
        if root_file.is_file() and legacy_file.is_file() and root_file.read_bytes() == legacy_file.read_bytes():
            rep.warn('plugin.json and .codex-plugin/plugin.json are byte-identical; the portable root manifest and the legacy manifest place requires_local_executor differently')
    except OSError as exc:
        rep.error('cannot compare Codex manifests: %s' % exc)

    market = load_json(root, MARKETPLACE, rep, required=False)
    if market is not None:
        check_manifest_claims(root, MARKETPLACE, market, rep, has_license)
        plugins = market.get('plugins')
        if not isinstance(plugins, list) or not plugins:
            rep.error('%s: plugins must be a non-empty list' % MARKETPLACE)
        else:
            for entry in plugins:
                if not isinstance(entry, dict):
                    rep.error('%s: plugin entry must be an object' % MARKETPLACE)
                    continue
                if entry.get('name') != PLUGIN_NAME:
                    rep.error('%s: plugin name must be %r, got %r' % (MARKETPLACE, PLUGIN_NAME, entry.get('name')))
                ev = entry.get('version')
                if ev is not None and versions and ev not in versions.values():
                    rep.error('%s: plugin version %r disagrees with manifests %s' % (MARKETPLACE, ev, sorted(set(versions.values()))))
                src = entry.get('source')
                remote = (isinstance(src, dict) and bool(src.get('url'))) or (isinstance(src, str) and re.match(r'^[a-z]+://', src) is not None)
                if remote:
                    rep.warn('%s: plugin source is a remote URL; this package is local-only and is not published' % MARKETPLACE)
    version = next(iter(versions.values())) if versions and len(set(versions.values())) == 1 else None
    return version, hosts


def validate_skills(root, rep):
    sdir = root / 'skills'
    if link_problem(root, 'skills'):
        rep.error('symlinked directory refused: skills')
        return
    if not sdir.is_dir():
        rep.error('missing skills/ directory')
        return
    names = {}
    for skill in REQUIRED_SKILLS:
        if not (sdir / skill / 'SKILL.md').is_file():
            rep.error('missing required skill: skills/%s/SKILL.md' % skill)
    for md in sorted(sdir.glob('*/SKILL.md')):
        rel = md.relative_to(root).as_posix()
        bad = link_problem(root, rel)
        if bad:
            rep.error('%s: symlink in path refused: %s' % (rel, bad))
            continue
        text = md.read_text(encoding='utf-8', errors='replace')
        meta = parse_frontmatter(text)
        if meta is None:
            rep.error('%s: missing or unterminated YAML frontmatter' % rel)
            continue
        for key in ('name', 'description'):
            if not meta.get(key):
                rep.error('%s: frontmatter lacks non-empty %r' % (rel, key))
        nm = meta.get('name')
        if nm:
            if nm != md.parent.name:
                rep.error('%s: frontmatter name %r must equal its directory name %r' % (rel, nm, md.parent.name))
            if nm in names:
                rep.error('%s: skill name %r duplicates %s' % (rel, nm, names[nm]))
            names[nm] = rel


def iter_candidates(root, rep):
    seen = set()

    def consider(path):
        rel = path.relative_to(root).as_posix()
        if rel in seen:
            return None
        seen.add(rel)
        problem = path_problem(rel)
        if problem:
            rep.error('unsafe path %r: %s' % (rel, problem))
            return None
        bad = link_problem(root, rel)
        if bad:
            rep.error('symlink refused: %s (symlink component: %s)' % (rel, bad))
            return None
        if secret_filename(path.name):
            rep.error('secret-looking filename refused: %s' % rel)
            return None
        if path.name in EXCLUDE_FILE_NAMES or path.suffix == '.pyc':
            return None
        if path.suffix.lower() not in TEXT_EXTENSIONS and not path.name.startswith(('LICENSE', 'NOTICE')):
            rep.warn('skipped non-text/unknown file type: %s' % rel)
            return None
        if not path.is_file():
            return None
        if path.stat().st_size > MAX_FILE_BYTES:
            rep.error('file too large (> %d bytes): %s' % (MAX_FILE_BYTES, rel))
            return None
        return rel

    out = []
    for d in INCLUDE_DIRS:
        base = root / d
        if base.is_symlink():
            rep.error('symlinked directory refused: %s' % d)
            continue
        if not base.is_dir():
            continue
        for dirpath, dirnames, filenames in os.walk(base, followlinks=False):
            keep = []
            for n in sorted(dirnames):
                sub = Path(dirpath) / n
                if n in EXCLUDE_DIR_NAMES:
                    continue
                if sub.is_symlink():
                    rep.error('symlinked directory refused: %s' % sub.relative_to(root).as_posix())
                    continue
                keep.append(n)
            dirnames[:] = keep
            for fn in sorted(filenames):
                rel = consider(Path(dirpath) / fn)
                if rel:
                    out.append(rel)
    for f in INCLUDE_FILES:
        p = root / f
        if p.is_file() or p.is_symlink():
            rel = consider(p)
            if rel:
                out.append(rel)
    for pattern in INCLUDE_GLOBS:
        for p in sorted(root.glob(pattern)):
            if p.is_file() or p.is_symlink():
                rel = consider(p)
                if rel:
                    out.append(rel)
    result = sorted(set(out))
    folded = {}
    for rel in result:
        key = collision_key(rel)
        if key in folded:
            rep.error('path collision (case or Unicode normalization): %s vs %s' % (shown(folded[key]), shown(rel)))
        folded[key] = rel
    for key in conflict_keys(folded):
        rep.error('path conflict: %s is both a file and a directory prefix' % shown(folded[key]))
    if len(result) > MAX_FILES:
        rep.error('too many files (%d > %d)' % (len(result), MAX_FILES))
    return result


def read_contents(root, rels, rep):
    '''Read every packaged file exactly once; hashes, scans and archive use these bytes.'''
    contents = {}
    total = 0
    for rel in rels:
        try:
            data = read_regular(root, rel)
        except OSError as exc:
            rep.error('cannot read %s: %s' % (rel, exc))
            continue
        if len(data) > MAX_FILE_BYTES:
            rep.error('file too large (> %d bytes): %s' % (MAX_FILE_BYTES, rel))
            continue
        total += len(data)
        contents[rel] = data
    if total > MAX_TOTAL_BYTES:
        rep.error('package too large (%d > %d bytes)' % (total, MAX_TOTAL_BYTES))
    return contents


def scan_content(contents, rep):
    for rel in sorted(contents):
        data = contents[rel]
        if data[:4] in BINARY_MAGIC:
            rep.error('native executable content: %s' % rel)
            continue
        if bytes(1) in data:
            rep.error('binary content in text file: %s' % rel)
            continue
        try:
            text = data.decode('utf-8')
        except UnicodeDecodeError:
            rep.warn('not valid UTF-8: %s' % rel)
            text = data.decode('utf-8', errors='replace')
        for label, rx in SECRET_PATTERNS:
            if rx.search(text):
                rep.error('possible %s in %s' % (label, rel))
        if LOCAL_PATH.search(text):
            rep.warn('machine-specific absolute path in %s' % rel)


def source_tree_digest(contents):
    '''SHA-256 over the sorted lines of file digest, two spaces, path for every packaged file.'''
    lines = ''.join('%s  %s%s' % (sha256_bytes(contents[rel]), rel, NL) for rel in sorted(contents))
    return sha256_bytes(lines.encode('utf-8'))


def binary_identity(path, root, rep):
    '''Describe an explicitly supplied runtime binary for the release manifest.
    It is hashed only: never executed, never bundled, and its directory is not recorded.'''
    if not path.is_absolute():
        rep.error('--binary must be an absolute path: %s' % shown(path))
        return None
    if path_problem(path.name):
        rep.error('--binary: unsafe file name %s' % shown(path.name))
        return None
    try:
        mode = os.lstat(str(path)).st_mode
    except OSError as exc:
        rep.error('--binary: cannot inspect (%s)' % (exc.strerror or type(exc).__name__))
        return None
    if stat.S_ISLNK(mode):
        rep.error('--binary: symlink refused; pass the real file')
        return None
    if not stat.S_ISREG(mode):
        rep.error('--binary: not a regular file')
        return None
    if not mode & 0o111:
        rep.error('--binary: file is not executable')
        return None
    inside = True
    try:
        path.resolve().relative_to(root)
    except ValueError:
        inside = False
    if inside:
        rep.error('--binary: must live outside the plugin tree')
        return None
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_NONBLOCK', 0) | getattr(os, 'O_CLOEXEC', 0)
    try:
        with os.fdopen(os.open(str(path), flags), 'rb') as fh:
            if not stat.S_ISREG(os.fstat(fh.fileno()).st_mode):
                rep.error('--binary: not a regular file')
                return None
            head = fh.read(4)
            digest = hashlib.sha256(head)
            size = len(head)
            for chunk in iter(lambda: fh.read(65536), b''):
                digest.update(chunk)
                size += len(chunk)
    except OSError as exc:
        rep.error('--binary: cannot read (%s)' % (exc.strerror or type(exc).__name__))
        return None
    if head not in BINARY_MAGIC:
        rep.warn('--binary does not start with a known native executable header; identity recorded anyway')
    return {
        'name': path.name,
        'sha256': digest.hexdigest(),
        'bytes': size,
        'bundled': False,
        'executed_by_packager': False,
        'built_from_go_source_verified': False,
    }


def go_source_digest(root, rep):
    '''SHA-256 over every .go, go.mod and go.sum file under root (links refused). This names
    the source next to a binary; it does not prove the binary was built from it.'''
    entries = []
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
        dirnames[:] = sorted(n for n in dirnames if n not in EXCLUDE_DIR_NAMES and not (Path(dirpath) / n).is_symlink())
        for fn in sorted(filenames):
            if not (fn.endswith('.go') or fn in ('go.mod', 'go.sum')):
                continue
            rel = (Path(dirpath) / fn).relative_to(root).as_posix()
            try:
                data = read_regular(root, rel)
            except OSError as exc:
                rep.error('go source %s: %s' % (shown(rel), exc))
                continue
            entries.append((rel, sha256_bytes(data)))
    if not entries:
        rep.warn('--binary given but no Go sources found under the root; go_source_sha256 omitted')
        return None
    lines = ''.join('%s  %s%s' % (h, r, NL) for r, h in sorted(entries))
    return sha256_bytes(lines.encode('utf-8'))


def epoch():
    try:
        val = int(os.environ.get('SOURCE_DATE_EPOCH', ZIP_MIN_EPOCH))
    except ValueError:
        return ZIP_MIN_EPOCH
    return min(max(val, ZIP_MIN_EPOCH), ZIP_MAX_EPOCH)


def write_zip(contents, prefix, dest):
    stamp = time.gmtime(epoch())[:6]
    with zipfile.ZipFile(dest, 'w', zipfile.ZIP_DEFLATED) as zf:
        for rel in sorted(contents):
            info = zipfile.ZipInfo(prefix + '/' + rel, date_time=stamp)
            info.create_system = 3
            info.external_attr = 0o100644 << 16
            zf.writestr(info, contents[rel], compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)


def write_targz(contents, prefix, dest):
    with contextlib.nullcontext(dest) as raw:
        with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0, compresslevel=9) as gz:
            with tarfile.open(fileobj=gz, mode='w', format=tarfile.PAX_FORMAT) as tf:
                for rel in sorted(contents):
                    data = contents[rel]
                    ti = tarfile.TarInfo(prefix + '/' + rel)
                    ti.size = len(data)
                    ti.mtime = epoch()
                    ti.mode = 0o644
                    ti.type = tarfile.REGTYPE
                    ti.uid = 0
                    ti.gid = 0
                    ti.uname = ''
                    ti.gname = ''
                    tf.addfile(ti, io.BytesIO(data))


def read_archive(path, rep):
    '''Read a zip or tar.gz without extracting to disk. Returns [(name, bytes)] or None.
    Refuses directories, links, devices, encrypted members and oversize content.'''
    members = []
    total = 0
    try:
        if zipfile.is_zipfile(path):
            with zipfile.ZipFile(path) as zf:
                infos = zf.infolist()
                if len(infos) > MAX_FILES:
                    rep.error('archive has too many members (%d > %d)' % (len(infos), MAX_FILES))
                    return None
                for info in infos:
                    name = info.filename
                    mode = (info.external_attr >> 16) & 0o170000
                    if info.is_dir():
                        rep.error('directory member refused: %r' % name)
                        continue
                    if mode not in (0, 0o100000):
                        rep.error('non-regular member refused (mode %o): %r' % (mode, name))
                        continue
                    if info.flag_bits & 0x1:
                        rep.error('encrypted member refused: %r' % name)
                        continue
                    if info.file_size > MAX_FILE_BYTES:
                        rep.error('member too large: %r' % name)
                        continue
                    with zf.open(info) as fh:
                        data = fh.read(MAX_FILE_BYTES + 1)
                    if len(data) != info.file_size:
                        rep.error('member size does not match its header: %r' % name)
                        continue
                    total += len(data)
                    if total > MAX_TOTAL_BYTES:
                        rep.error('archive expands beyond %d bytes' % MAX_TOTAL_BYTES)
                        return None
                    members.append((name, data))
        elif tarfile.is_tarfile(path):
            with tarfile.open(path, 'r:gz') as tf:
                count = 0
                for m in tf:
                    count += 1
                    if count > MAX_FILES:
                        rep.error('archive has too many members (> %d)' % MAX_FILES)
                        return None
                    if not m.isreg():
                        rep.error('non-regular member refused (type %r): %r' % (m.type, m.name))
                        continue
                    if m.size > MAX_FILE_BYTES:
                        rep.error('member too large: %r' % m.name)
                        continue
                    fh = tf.extractfile(m)
                    data = fh.read(MAX_FILE_BYTES + 1) if fh is not None else b''
                    if len(data) != m.size:
                        rep.error('member size does not match its header: %r' % m.name)
                        continue
                    total += len(data)
                    if total > MAX_TOTAL_BYTES:
                        rep.error('archive expands beyond %d bytes' % MAX_TOTAL_BYTES)
                        return None
                    members.append((m.name, data))
        else:
            rep.error('not a zip or tar.gz archive: %s' % path)
            return None
    except (OSError, EOFError, ValueError, zlib.error, zipfile.BadZipFile, tarfile.TarError) as exc:
        rep.error('cannot read archive: %s' % exc)
        return None
    return members


def read_sidecar(folder, name, rep):
    '''Return (present, data). Symlinks and non-regular files are errors and are never
    followed. Sidecar content is never echoed into messages.'''
    path = folder / name
    try:
        mode = os.lstat(path).st_mode
    except FileNotFoundError:
        return False, None
    except OSError as exc:
        rep.error('%s: cannot inspect (%s)' % (name, exc.strerror or type(exc).__name__))
        return True, None
    if stat.S_ISLNK(mode):
        rep.error('%s: symlink refused; sidecars must be regular files' % name)
        return True, None
    if not stat.S_ISREG(mode):
        rep.error('%s: not a regular file' % name)
        return True, None
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_NONBLOCK', 0) | getattr(os, 'O_CLOEXEC', 0)
    try:
        with os.fdopen(os.open(str(path), flags), 'rb') as fh:
            if not stat.S_ISREG(os.fstat(fh.fileno()).st_mode):
                rep.error('%s: not a regular file' % name)
                return True, None
            data = fh.read(MAX_FILE_BYTES + 1)
    except OSError as exc:
        rep.error('%s: cannot read (%s)' % (name, exc.strerror or type(exc).__name__))
        return True, None
    if len(data) > MAX_FILE_BYTES:
        rep.error('%s: too large (> %d bytes)' % (name, MAX_FILE_BYTES))
        return True, None
    return True, data


def check_sidecars(archive, contents, rep, require=False):
    folder = archive.parent
    sums_present, sums_data = read_sidecar(folder, 'SHA256SUMS', rep)
    rel_present, rel_data = read_sidecar(folder, 'release-manifest.json', rep)
    archive_sha = sha256_file(archive)
    if sums_data is not None:
        listed = {}
        text = sums_data.decode('utf-8', errors='replace')
        for num, line in enumerate(text.splitlines(), 1):
            if not line.strip():
                continue
            m = re.fullmatch(r'([0-9a-f]{64}) {2}([A-Za-z0-9._+-]+)', line)
            if not m:
                rep.error('SHA256SUMS: malformed line %d' % num)
                continue
            if m.group(2) in listed:
                rep.error('SHA256SUMS: duplicate entry on line %d' % num)
                continue
            listed[m.group(2)] = m.group(1)
        if listed.get(archive.name) != archive_sha:
            rep.error('SHA256SUMS: no matching digest for %s' % archive.name)
        if rel_data is not None and listed.get('release-manifest.json') != sha256_bytes(rel_data):
            rep.error('SHA256SUMS: digest of release-manifest.json does not match')
    elif not sums_present:
        if require:
            rep.error('SHA256SUMS not found next to the archive (--require-sidecars)')
        else:
            rep.warn('SHA256SUMS not found next to the archive; digests were not cross-checked')
    if rel_data is None:
        if not rel_present:
            if require:
                rep.error('release-manifest.json not found next to the archive (--require-sidecars)')
            else:
                rep.warn('release-manifest.json not found next to the archive; file list was not cross-checked')
        return
    try:
        release = json.loads(rel_data.decode('utf-8'))
    except (UnicodeDecodeError, ValueError, RecursionError) as exc:
        rep.error('release-manifest.json: cannot parse as UTF-8 JSON (%s)' % type(exc).__name__)
        return
    if not isinstance(release, dict):
        rep.error('release-manifest.json: top-level value must be an object')
        return
    if release.get('archive') != archive.name:
        rep.error('release-manifest.json: archive name does not match %s' % archive.name)
    if release.get('archive_sha256') != archive_sha:
        rep.error('release-manifest.json: archive_sha256 does not match the archive')
    tree_claim = release.get('source_tree_sha256')
    if tree_claim is None:
        if require:
            rep.error('release-manifest.json: source_tree_sha256 missing (--require-sidecars)')
        else:
            rep.warn('release-manifest.json: source_tree_sha256 missing; source identity was not cross-checked')
    elif tree_claim != source_tree_digest(contents):
        rep.error('release-manifest.json: source_tree_sha256 does not match the archived files')
    if release.get('runtime_binary_bundled') is not False:
        rep.error('release-manifest.json: runtime_binary_bundled must be false for a source-only archive')
    runtime = release.get('runtime_binary')
    if runtime is not None and not (isinstance(runtime, dict) and isinstance(runtime.get('sha256'), str)
                                    and re.fullmatch(r'[0-9a-f]{64}', runtime['sha256']) and runtime.get('bundled') is False):
        rep.error('release-manifest.json: runtime_binary must record a SHA-256 and bundled false')
    files = release.get('files')
    if not isinstance(files, list):
        rep.error('release-manifest.json: files must be a list')
        return
    declared = {}
    for entry in files:
        if not (isinstance(entry, dict) and isinstance(entry.get('path'), str)):
            rep.error('release-manifest.json: malformed file entry')
            continue
        if entry['path'] in declared:
            rep.error('release-manifest.json: duplicate file entry')
            continue
        declared[entry['path']] = entry.get('sha256')
    actual = dict((rel, sha256_bytes(data)) for rel, data in contents.items())
    only_listed = set(declared) - set(actual)
    only_archived = set(actual) - set(declared)
    if only_listed or only_archived:
        rep.error('release-manifest.json: file list differs from archive (%d only listed, %d only archived%s)' % (
            len(only_listed), len(only_archived),
            ''.join('; archive-only %s' % shown(n) for n in sorted(only_archived)[:5])))
    else:
        for rel in sorted(actual):
            if declared[rel] != actual[rel]:
                rep.error('release-manifest.json: sha256 mismatch for %s' % shown(rel))


def verify_archive(archive, rep, require_sidecars=False):
    '''Audit an archive: names, allowlist, content scan, manifests, skills, sidecars.
    Writes only into a private temporary directory. Returns contents or None.'''
    members = read_archive(archive, rep)
    if members is None:
        return None
    if not members:
        rep.error('archive is empty')
        return None
    before = len(rep.errors)
    prefixes = set()
    contents = {}
    folded = {}
    for name, data in members:
        problem = path_problem(name)
        if problem:
            rep.error('unsafe member name %r: %s' % (name, problem))
            continue
        parts = name.split('/')
        if len(parts) < 2:
            rep.error('member outside the single top-level directory: %r' % name)
            continue
        prefixes.add(parts[0])
        rel = '/'.join(parts[1:])
        key = collision_key(rel)
        if key in folded:
            rep.error('path collision (duplicate, case or Unicode normalization): %s vs %s' % (shown(folded[key]), shown(rel)))
            continue
        folded[key] = rel
        if secret_filename(parts[-1]):
            rep.error('secret-looking member filename refused: %s' % shown(name))
            continue
        if not allowed_rel(rel):
            rep.error('member outside the packaging allowlist: %r' % name)
            continue
        contents[rel] = data
    for key in conflict_keys(folded):
        rep.error('path conflict: %s is both a file and a directory prefix' % shown(folded[key]))
    if len(prefixes) != 1:
        rep.error('archive must have exactly one top-level directory; found %d conflicting prefixes: %s' % (
            len(prefixes), ', '.join(shown(p, 40) for p in sorted(prefixes)[:3])))
    if len(rep.errors) > before:
        return None
    prefix = next(iter(prefixes))
    scan_content(contents, rep)
    with tempfile.TemporaryDirectory(prefix='irrigator-verify-') as tmp:
        troot = Path(tmp)
        for rel, data in contents.items():
            dest = troot / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(data)
        version, hosts = validate_manifests(troot, rep)
        validate_skills(troot, rep)
    if version is None:
        rep.error('archive manifests do not give one consistent version')
    elif prefix != '%s-plugin-%s' % (PLUGIN_NAME, version):
        rep.error('top-level directory %r does not match manifest version %s' % (prefix, version))
    check_sidecars(archive, contents, rep, require=require_sidecars)
    return contents


def check_output_dir(root, raw_out, rep):
    if raw_out.is_symlink():
        rep.error('output directory must not be a symlink: %s' % raw_out)
    out = raw_out.resolve()
    try:
        rel = out.relative_to(root)
    except ValueError:
        return out
    if not rel.parts:
        rep.error('output directory must not be the plugin root')
    elif rel.parts[0] in INCLUDE_DIRS or rel.parts[0] in ('verification', 'tools'):
        rep.error('output directory %s sits inside a packaged directory' % rel.as_posix())
    return out


def create_exclusive(path):
    '''Create a brand-new file for writing. Never follows a link and never reuses or
    overwrites an existing path of any kind (FileExistsError instead).'''
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_CLOEXEC', 0)
    return os.fdopen(os.open(str(path), flags, 0o644), 'wb')


def emit(rep, strict):
    for w in rep.warnings:
        print('warning: %s' % w, file=sys.stderr)
    for e in rep.errors:
        print('error: %s' % e, file=sys.stderr)
    return bool(rep.errors) or (strict and bool(rep.warnings))


def main(argv):
    here = Path(__file__).resolve().parent.parent
    ap = argparse.ArgumentParser(description='Package the Irrigator plugin locally (no publish, no install).')
    ap.add_argument('--root', type=Path, default=here, help='plugin tree (default: parent of tools/)')
    ap.add_argument('--out', type=Path, help='output directory (default: ROOT/dist)')
    ap.add_argument('--format', choices=('zip', 'tar.gz'), default='zip')
    ap.add_argument('--version', help='override version label; must equal manifest version when one exists')
    ap.add_argument('--check', action='store_true', help='validate only; write nothing')
    ap.add_argument('--list', action='store_true', help='print files that would be packaged; write nothing')
    ap.add_argument('--verify', type=Path, metavar='ARCHIVE', help='audit an existing zip or tar.gz; write nothing but a temp dir')
    ap.add_argument('--force', action='store_true', help='overwrite existing artifacts in --out')
    ap.add_argument('--binary', type=Path, metavar='ABS_PATH', help='record SHA-256/size of the runtime binary you built (absolute path outside the tree); it is never executed or bundled')
    ap.add_argument('--strict', action='store_true', help='treat warnings as errors')
    ap.add_argument('--require-sidecars', action='store_true', help='with --verify: a missing SHA256SUMS or release-manifest.json is an error, not a warning')
    args = ap.parse_args(argv)
    if args.require_sidecars and args.verify is None:
        print('error: --require-sidecars only applies to --verify', file=sys.stderr)
        return 2

    if args.verify is not None:
        if args.check or args.list or args.out or args.binary:
            print('error: --verify cannot be combined with --check, --list, --out or --binary', file=sys.stderr)
            return 2
        if args.verify.is_symlink() or not args.verify.is_file():
            print('error: not a regular file: %s' % args.verify, file=sys.stderr)
            return 2
        rep = Report()
        verified = verify_archive(args.verify, rep, require_sidecars=args.require_sidecars)
        failed = emit(rep, args.strict)
        print('%s: %d files, %d errors, %d warnings' % (
            'FAIL' if failed else 'ok', len(verified or {}), len(rep.errors), len(rep.warnings)))
        return 1 if failed else 0

    root = args.root.resolve()
    if not root.is_dir():
        print('error: root is not a directory: %s' % root, file=sys.stderr)
        return 2
    building = not (args.check or args.list)

    rep = Report()
    manifest_version, hosts = validate_manifests(root, rep)
    validate_skills(root, rep)
    files = iter_candidates(root, rep)
    contents = read_contents(root, files, rep)
    files = sorted(contents)
    scan_content(contents, rep)
    if not any(root.glob('LICENSE*')):
        rep.warn('no LICENSE file: no license is chosen without authorization; archive carries none')

    version = args.version or manifest_version
    if args.version and manifest_version and args.version != manifest_version:
        rep.error('--version %r disagrees with manifest version %r' % (args.version, manifest_version))
    if not version:
        rep.error('cannot determine version (no consistent manifest version and no --version)')
    elif not VERSION_LABEL.fullmatch(version):
        rep.error('unsafe version label: %r' % version)
    binary_info = None
    go_digest = None
    if args.binary is not None:
        binary_info = binary_identity(args.binary, root, rep)
        go_digest = go_source_digest(root, rep)
    out = None
    if building:
        out = check_output_dir(root, args.out or (root / 'dist'), rep)

    failed = emit(rep, args.strict)

    if args.list:
        for rel in files:
            print(rel)
    if args.check or args.list:
        print('%s: %d files, %d errors, %d warnings' % ('FAIL' if failed else 'ok', len(files), len(rep.errors), len(rep.warnings)))
        return 1 if failed else 0
    if failed:
        print('refusing to package: validation failed', file=sys.stderr)
        return 1
    if not files:
        print('error: nothing to package', file=sys.stderr)
        return 2

    prefix = '%s-plugin-%s' % (PLUGIN_NAME, version)
    archive = out / ('%s.%s' % (prefix, args.format))
    sums = out / 'SHA256SUMS'
    relman = out / 'release-manifest.json'
    handles = {}
    created = []
    try:
        out.mkdir(parents=True, exist_ok=True)
        existing = [p for p in (archive, sums, relman) if p.exists() or p.is_symlink()]
        if existing and not args.force:
            print('error: refusing to overwrite (use --force): ' + ', '.join(p.name for p in existing), file=sys.stderr)
            return 2
        for p in existing:
            if p.is_symlink() or p.is_dir():
                print('error: refusing to replace a symlink or directory: %s' % p, file=sys.stderr)
                return 2
        part = out / (archive.name + '.part')
        relpart = out / (relman.name + '.part')
        sumspart = out / (sums.name + '.part')
        for target in (part, relpart, sumspart):
            try:
                handles[target] = create_exclusive(target)
            except FileExistsError:
                print('error: refusing to reuse an existing temporary path (inspect it and remove it yourself if stale): %s' % target.name, file=sys.stderr)
                return 2
            created.append(target)
        with handles[part] as fh:
            (write_zip if args.format == 'zip' else write_targz)(contents, prefix, fh)
        check = Report()
        loaded = read_archive(part, check)
        expect = dict((prefix + '/' + rel, data) for rel, data in contents.items())
        if check.errors or loaded is None or dict(loaded) != expect:
            print('error: archive round-trip check failed; nothing was kept', file=sys.stderr)
            return 2
        archive_sha = sha256_file(part)
        entries = [{'path': rel, 'sha256': sha256_bytes(contents[rel]), 'bytes': len(contents[rel])} for rel in files]
        release = {
            'name': PLUGIN_NAME,
            'version': version,
            'archive': archive.name,
            'archive_sha256': archive_sha,
            'format': args.format,
            'source_date_epoch': epoch(),
            'scope': 'local-only; not published, not installed',
            'host_install_tested': False,
            'host_manifests': hosts,
            'runtime_binary_bundled': False,
            'runtime_binary': binary_info,
            'go_source_sha256': go_digest,
            'source_tree_sha256': source_tree_digest(contents),
            'mcp_server': None,
            'license_file_included': any(f.startswith('LICENSE') for f in files),
            'files': entries,
        }
        relman_bytes = (json.dumps(release, indent=2, sort_keys=True) + NL).encode('utf-8')
        sums_bytes = ('%s  %s%s%s  %s%s' % (archive_sha, archive.name, NL, sha256_bytes(relman_bytes), relman.name, NL)).encode('utf-8')
        for target, blob in ((relpart, relman_bytes), (sumspart, sums_bytes)):
            with handles[target] as fh:
                fh.write(blob)
        os.replace(relpart, relman)
        os.replace(sumspart, sums)
        os.replace(part, archive)
    except OSError as exc:
        print('error: %s' % exc, file=sys.stderr)
        return 2
    finally:
        for fh in handles.values():
            fh.close()
        for target in created:
            try:
                os.unlink(str(target))
            except OSError:
                pass
    print('wrote %s' % archive)
    print('wrote %s' % sums)
    print('wrote %s' % relman)
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
