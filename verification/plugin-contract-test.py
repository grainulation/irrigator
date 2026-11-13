#!/usr/bin/env python3
"""Irrigator plugin contract test (stdlib only, local only, read-only by default).

Usage (run from anywhere; -I keeps the interpreter from importing planted modules):

    python3 -I verification/plugin-contract-test.py [--root DIR] [--json] [--strict]
            [--allow-missing] [--binary /abs/path/irrigator] [--smoke]
            [--archive RELEASE.tar.gz|.zip]

What it checks
  static (default): manifests, skill frontmatter + required workflow concepts,
  every documented `irrigator ...` command/flag against the real CLI surface
  (derived from main.go of 1.0.0-rc.4), shape arithmetic, display-metadata
  limits in JSON examples, unsupported claims (MCP, license, pricing, remote
  publication, Grainulator vocabulary), tree hygiene, packaging-tool hygiene.
  --archive RELEASE: scans the actual members of a .zip/.tar/.tar.gz release
  archive (member names, credential-like files, native executables, link and
  path escapes) with bounded temporary extraction for static documentation checks. Archive-only
  invocation checks archive content, never unrelated checkout documentation. Without --archive the hygiene scan covers exactly --root; a defaulted
  root (the checkout) is reported as a WARN because a checkout is not release
  content. Real binary contamination in the scanned scope is always a FAIL.
  --binary: compares `--help`/`version`/`doctor` of a built irrigator against
  the documented command set.
  --smoke (needs --binary): credential-free, mock-backend run inside a fresh
  temporary directory. Never contacts a model, network, or remote; never
  writes into --root.

Exit status: 0 = no FAIL (and no WARN with --strict), 1 = FAIL, 2 = usage error.
This script never installs into a host, pushes, publishes, or edits files.
"""
import argparse
import importlib.util
import json
import os
import re
import shlex
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path

SKILLS = ["irrigator", "router", "plan", "run", "resume", "observe", "debug", "budget", "swarm"]

# Printed with every report so a static pass is never read as host discovery.
SCOPE_NOTE = ('repository-or-package-root static checks (files and CLI contract); native Codex and '
              'Claude Code plugin discovery is not exercised')

# Required package files.
FROZEN_FILES = [
    ".claude-plugin/plugin.json", ".claude-plugin/marketplace.json", "docs/claude-plugin.md",
    ".codex-plugin/plugin.json", "plugin.json", "docs/codex-plugin.md",
    "skills/irrigator/SKILL.md", "skills/router/SKILL.md",
    "skills/plan/SKILL.md", "skills/run/SKILL.md",
    "skills/resume/SKILL.md", "skills/observe/SKILL.md", "skills/budget/SKILL.md",
    "skills/debug/SKILL.md",
    "skills/swarm/SKILL.md",
    "README.md", "docs/runtime-notation.md", "docs/plugin-architecture.md",
    "tools/package-plugin.py", "docs/local-release.md",
    "CONTRIBUTING.md", "SECURITY.md", "docs/license-inventory.md", "verification/cli-contract.json", "docs/plan.schema.json",
]

# Generated from actual runtime FlagSets. The Go snapshot test rejects drift;
# binary-backed checks additionally compare the supplied executable's metadata.
CONTRACT_PATH = Path(__file__).with_name("cli-contract.json")

def parse_cli_contract(value):
    if not isinstance(value, dict) or value.get("schema") != "irrigator.cli/v1" or not isinstance(value.get("commands"), list):
        raise ValueError("unsupported CLI metadata schema")
    flags, subs, hidden = {}, {}, set()
    for command in value["commands"]:
        name = command.get("name")
        if not isinstance(name, str) or not re.fullmatch(r"[a-z][a-z-]*", name) or name in flags:
            raise ValueError("invalid or duplicate CLI command")
        entries = command.get("flags")
        if not isinstance(entries, list):
            raise ValueError("CLI flags must be an array")
        names = [entry.get("name") for entry in entries]
        if any(not isinstance(x, str) or not re.fullmatch(r"[a-z][a-z0-9_-]*", x) for x in names) or len(set(names)) != len(names):
            raise ValueError("invalid or duplicate CLI flag")
        flags[name] = set(names)
        children = command.get("subcommands", [])
        if not isinstance(children, list) or any(not isinstance(x, str) or not re.fullmatch(r"[a-z][a-z-]*", x) for x in children):
            raise ValueError("invalid CLI subcommands")
        if children:
            subs[name] = set(children)
        if command.get("hidden"):
            hidden.add(name)
    if not flags:
        raise ValueError("empty CLI metadata")
    return flags, subs, hidden

COMMAND_FLAGS, SUBCOMMANDS, HIDDEN_ALIASES = parse_cli_contract(json.loads(CONTRACT_PATH.read_text(encoding="utf-8")))
LENIENT = set()  # Every flag now comes from the runtime, including panel and serve.
HELP_COMMANDS = [c for c in COMMAND_FLAGS if c != "help" and c not in HIDDEN_ALIASES]

NEG = re.compile(r"\b(not|never|no|without|don't|doesn't|cannot|can't|forbid\w*|prohibit\w*|avoid|"
                 r"unless|until|nothing|neither|nor)\b", re.I)
FENCE = re.compile(r"```([A-Za-z0-9_+-]*)[^\n]*\n(.*?)```", re.S)
# "Grainulator" is the separate planning product that must not leak into Irrigator
# manifests, skills or docs. "Grainulation" (the legitimate author/organization name)
# is a different word and must not trip the rule.
GRAINULATOR_RE = re.compile(r"grainulat(?!ion)", re.I)

# Honest host-permission disclaimers ("Host permission prompts apply") are accepted.
# Invented bypass or ownership claims are not, unless the bypass word is directly negated.
HOST_PERMISSION_RE = re.compile(
    r"\bhost(?:'s|\u2019s)?\b[^.\n;]{0,60}?\b(?:permission|approval)s?\b"
    r"|\b(?:permission|approval)s?\b[^.\n;]{0,40}?\b(?:of|from|by|in) (?:the |your )?host\b", re.I)
BYPASS_RE = re.compile(
    r"\b(?:bypass\w*|circumvent\w*|overrid\w*|replac\w*|skip\w*|suppress\w*|disabl\w*|silenc\w*|"
    r"elevat\w*|broker\w*|auto-?approv\w*|pre-?approv\w*|unattended|"
    r"without\s+(?:asking|approval|prompt\w*|confirmation|consent))\b"
    r"|\bown\s+permissions?\s+(?:model|broker|system|layer|grants?)\b", re.I)
LIST_START_RE = re.compile(r"^\s*(?:[-*+>#|]|\d+[.)])\s")


def negated_before(text, start):
    """True when a negation directly precedes `start` inside the same clause."""
    clause = re.split(r"[,;:]", text[:start])[-1]
    return bool(NEG.search(" ".join(clause.split()[-4:])))


def permission_sentence(lines, idx, start):
    """The sentence around a match, joining wrapped neighbour lines but not list items."""
    line = lines[idx]
    prev = lines[idx - 1] if idx > 0 else ""
    nxt = lines[idx + 1] if idx + 1 < len(lines) else ""
    if not prev.strip() or LIST_START_RE.match(line) or prev.strip().startswith("```"):
        prev = ""
    if not nxt.strip() or LIST_START_RE.match(nxt) or nxt.strip().startswith("```"):
        nxt = ""
    window = prev + " " + line + " " + nxt
    pos = len(prev) + 1 + start
    begin = 0
    for m in re.finditer(r"[.!?;](?=\s)", window[:pos]):
        begin = m.end()
    m = re.search(r"[.!?;](?=\s|$)", window[pos:])
    end = pos + m.end() if m else len(window)
    return window[begin:end]


def host_owned_permission(text, match):
    # Attribute only this precise possessive phrase to the host, never the plugin.
    return (match.group().lower().startswith("own ") and
            re.search(r"\bhost(?:'s|’s)\s+$", text[:match.start()], re.I) is not None)


def unnegated_bypass(sentence):
    return any(not host_owned_permission(sentence, m) and
               not negated_before(sentence, m.start()) for m in BYPASS_RE.finditer(sentence))


def classify_permission_claim(lines, idx, start):
    """'bypass' (invented claim), 'honest' (host approval disclaimer) or 'plain'."""
    sentence = permission_sentence(lines, idx, start)
    if unnegated_bypass(sentence):
        return "bypass"
    return "honest" if HOST_PERMISSION_RE.search(sentence) else "plain"


DENIED_CONTEXT_RE = re.compile(r"\b(?:denied|deny|denies|refus\w*|reject\w*|blocked?|caught|forbidden)\b", re.I)
MCP_BACKING_RE = re.compile(r"\(?(?:\.\./|docs/)?mcp\.md\)?|`irrigator mcp\b")


def mcp_backed(root):
    """MCP claims are allowed only while docs/mcp.md and the `mcp` CLI command exist."""
    try:
        contract = json.loads((Path(root) / "verification/cli-contract.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    names = {c.get("name") for c in contract.get("commands", []) if isinstance(c, dict)}
    return "mcp" in names and (Path(root) / "docs/mcp.md").is_file()


def mcp_server_entry_ok(entry):
    """The shipped plugin may register only the read-only `irrigator mcp` server."""
    if not isinstance(entry, dict) or set(entry) - {"command", "args"}:
        return False
    args = entry.get("args")
    return (entry.get("command") == "irrigator" and isinstance(args, list)
            and args[:1] == ["mcp"] and all(isinstance(a, str) for a in args)
            and "--allow-control" not in args)


def claim_suppressed(tid, negatable, lines, idx, m, in_code, rel="", backed=False):
    line = lines[idx]
    if tid == "claims.mcp" and backed and (rel == "docs/mcp.md" or MCP_BACKING_RE.search(line)):
        return True
    if tid == "claims.remote-action" and any(DENIED_CONTEXT_RE.search(x) for x in lines[max(0, idx - 2):idx + 3]):
        return True
    if tid == "claims.permission-bypass" and host_owned_permission(line, m):
        return True
    if tid == "claims.permissions":
        verdict = classify_permission_claim(lines, idx, m.start())
        if verdict == "honest" or (verdict != "bypass" and re.search(r"\brespect (?:host )?permission prompts\b", permission_sentence(lines, idx, m.start()), re.I)):
            return True
        if verdict == "bypass":
            return False
        return bool(NEG.search(permission_sentence(lines, idx, m.start())))
    if negatable == "prefix":
        return negated_before(line, m.start())
    return bool(negatable and NEG.search(line) and not (in_code and tid == "claims.remote-action"))


SEMVER = re.compile(r"^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$")


class Reporter:
    def __init__(self):
        self.rows = []

    def add(self, level, tid, msg):
        self.rows.append({"level": level, "id": tid, "message": msg})

    def check(self, tid, ok, msg, level="FAIL"):
        self.add("PASS" if ok else level, tid, msg)
        return ok

    def count(self, level):
        return sum(1 for r in self.rows if r["level"] == level)


class Ctx:
    def __init__(self, root, args, rep):
        self.root, self.args, self.rep = root, args, rep
        self._cache = {}
        self.command_flags = COMMAND_FLAGS
        self.subcommands = SUBCOMMANDS

    def path(self, rel):
        return self.root / rel

    def exists(self, rel):
        return self.path(rel).is_file()

    def text(self, rel):
        if rel in self._cache:
            return self._cache[rel]
        p = self.path(rel)
        val = p.read_text(encoding="utf-8", errors="replace") if p.is_file() else None
        self._cache[rel] = val
        return val

    def need(self, rel, tid):
        """Return text or record missing (FAIL, or SKIP under --allow-missing)."""
        t = self.text(rel)
        if t is None:
            self.rep.add("SKIP" if self.args.allow_missing else "FAIL", tid, "missing file: " + rel)
        return t

    def json(self, rel, tid):
        t = self.need(rel, tid)
        if t is None:
            return None
        try:
            return json.loads(t)
        except ValueError as e:
            self.rep.add("FAIL", tid, "%s is not valid JSON: %s" % (rel, e))
            return None

    def md_files(self):
        """Markdown files in scope for claim scans (tests, test matrix excluded)."""
        out = []
        for p in source_files(self.root):
            if p.suffix.lower() != ".md":
                continue
            rel = p.relative_to(self.root).as_posix()
            if rel.startswith(("verification/", ".git/")) or rel == "docs/plugin-test-matrix.md":
                continue
            out.append(rel)
        return out


def source_files(root):
    """Prune generated checkout trees before descent; packages get no build exemption."""
    checkout = (root / ".git").exists() and (root / "go.mod").is_file() and (root / "main.go").is_file()
    for directory, dirs, files in os.walk(root, followlinks=False):
        relative = Path(directory).relative_to(root)
        dirs[:] = sorted(d for d in dirs if d not in {".git", "__pycache__", "node_modules"}
                         and not (checkout and relative == Path(".") and d in {"build", "dist", "runs", ".irrigator"}))
        for name in dirs:
            entry = Path(directory) / name
            if entry.is_symlink():
                yield entry
        for name in sorted(files):
            if checkout and relative == Path(".") and name == "irrigator":
                continue
            yield Path(directory) / name


def mit_license(root):
    path = root / "LICENSE"
    if not path.is_file():
        return False
    text = path.read_text(encoding="utf-8")
    return all(marker in text for marker in ("MIT License", "Permission is hereby granted, free of charge", "THE SOFTWARE IS PROVIDED"))


# ---------------------------------------------------------------- helpers
def parse_frontmatter(text):
    m = re.match(r"---\r?\n(.*?)\r?\n---\r?\n", text, re.S)
    if not m:
        return None
    meta, key = {}, None
    for line in m.group(1).splitlines():
        kv = re.match(r"^([A-Za-z_][A-Za-z0-9_-]*):\s*(.*)$", line)
        item = re.match(r"^\s+-\s+(.*)$", line)
        if kv:
            key, val = kv.group(1), kv.group(2).strip()
            if val.startswith("[") and val.endswith("]"):
                meta[key] = [v.strip().strip("'\"") for v in val[1:-1].split(",") if v.strip()]
            elif val:
                meta[key] = val.strip("'\"")
            else:
                meta[key] = []
        elif item and key is not None:
            if not isinstance(meta[key], list):
                meta[key] = [meta[key]]
            meta[key].append(item.group(1).strip().strip("'\""))
    return meta


def walk_json(obj, path=""):
    if isinstance(obj, dict):
        for k, v in obj.items():
            yield (path + "/" + k, k, v)
            yield from walk_json(v, path + "/" + k)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            yield from walk_json(v, "%s[%d]" % (path, i))


def strip_fences(text):
    return FENCE.sub(lambda m: "\n" * m.group(0).count("\n"), text)


OPS = {"|", "||", "&&", ";", ">", ">>", "<", "2>&1", "&"}


def parse_invocation(line):
    """Return token list starting at the `irrigator` token, or None."""
    try:
        toks = shlex.split(line, comments=True)
    except ValueError:
        toks = line.split()
    while toks and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", toks[0]):
        toks.pop(0)
    if toks and toks[0] == "$":
        toks.pop(0)
    if not toks or os.path.basename(toks[0]) != "irrigator":
        return None
    cut = []
    for t in toks:
        if t in OPS or t.startswith((">", "2>")):
            break
        cut.append(t)
    return cut


def extract_invocations(text):
    """Yield (lineno, tokens, origin) for fenced and inline `irrigator` commands."""
    found = []
    for m in FENCE.finditer(text):
        base = text.count("\n", 0, m.start(2)) + 1
        logical, start = "", None
        for i, raw in enumerate(m.group(2).splitlines()):
            line = raw.strip()
            if line.startswith("$ "):
                line = line[2:]
            if start is None:
                start = base + i
            if line.endswith("\\"):
                logical += line[:-1] + " "
                continue
            logical += line
            toks = parse_invocation(logical)
            if toks is not None:
                found.append((start, toks, "fence"))
            logical, start = "", None
    rest = strip_fences(text)
    for m in re.finditer(r"`([^`\n]*\birrigator\b[^`\n]*)`", rest):
        toks = parse_invocation(m.group(1))
        if toks is not None:
            found.append((rest.count("\n", 0, m.start()) + 1, toks, "inline"))
    return found


def validate_invocation(toks, command_flags=None, subcommands=None):
    """Return list of (level, message) problems for one parsed invocation."""
    command_flags = COMMAND_FLAGS if command_flags is None else command_flags
    subcommands = SUBCOMMANDS if subcommands is None else subcommands
    probs = []
    if len(toks) < 2:
        return probs
    cmd = toks[1]
    if cmd.startswith("-") or not re.fullmatch(r"[_A-Za-z][A-Za-z-]*", cmd) or cmd.isupper():
        return probs  # --help, placeholders such as COMMAND or <cmd>
    if cmd.startswith("_"):
        return [("FAIL", "internal command `%s` must not be documented" % cmd)]
    if cmd not in command_flags:
        return [("FAIL", "unknown irrigator command `%s`" % cmd)]
    if cmd in subcommands and len(toks) > 2 and not toks[2].startswith("-"):
        if toks[2] not in subcommands[cmd] and not toks[2].isupper():
            probs.append(("FAIL", "unknown %s subcommand `%s`" % (cmd, toks[2])))
    allowed = command_flags[cmd]
    names, positional = set(), 0
    for t in toks[2:]:
        if t.startswith("-") and t != "-":
            name = t.lstrip("-").split("=", 1)[0]
            names.add(name)
            if name in ("h", "help"):
                continue
            if name not in allowed:
                lvl = "WARN" if cmd in LENIENT else "FAIL"
                probs.append((lvl, "`%s` has no flag --%s" % (cmd, name)))
        else:
            positional += 1
    if cmd in ("apply", "prepare") and positional >= 1 and "run" not in names:
        probs.append(("FAIL", "`%s` requires --run" % cmd))
    if cmd in ("apply", "deliver", "resume") and {"budget", "call-budget"} <= names:
        probs.append(("FAIL", "choose --call-budget or its --budget alias, not both"))
    if cmd == "resolve" and positional >= 2 and "result" not in names:
        probs.append(("FAIL", "`resolve` requires --result"))
    return probs


def shape_total(a, d, s):
    return a * sum(s ** k for k in range(d))


def rune_len(s):
    return len(s) if isinstance(s, str) else -1


def validate_display_plan(plan):
    """Return list of problems for a parsed plan dict, per docs/public-display-metadata.md."""
    probs = []
    pd = plan.get("display")
    if pd is not None:
        if not isinstance(pd, dict):
            probs.append("plan.display must be an object")
        else:
            if pd.get("version") != 1:
                probs.append("plan.display.version must be 1")
            if not (0 < rune_len(pd.get("title", "")) <= 60) and "title" in pd:
                probs.append("plan.display.title must be 1..60 runes")
            if rune_len(pd.get("purpose", "")) > 140:
                probs.append("plan.display.purpose must be <=140 runes")
    for t in plan.get("tasks", []) or []:
        if not isinstance(t, dict):
            continue
        tid = t.get("id", "?")
        d = t.get("display")
        if d is not None:
            if not isinstance(d, dict):
                probs.append("task %s display must be an object" % tid)
                continue
            for key, lim in (("title", 48), ("why_spawned", 100), ("done_when", 140)):
                if key in d and rune_len(d[key]) > lim:
                    probs.append("task %s display.%s exceeds %d runes" % (tid, key, lim))
            of = d.get("outcome_field", "")
            if of:
                if of not in (t.get("requires") or []):
                    probs.append("task %s outcome_field %r not in requires" % (tid, of))
                if of in (t.get("file_fields") or []):
                    probs.append("task %s outcome_field %r is a file_fields entry" % (tid, of))
        sp = t.get("spawn")
        if isinstance(sp, dict) and sp.get("field") and sp["field"] not in (t.get("requires") or []):
            probs.append("task %s spawn.field %r not in requires" % (tid, sp["field"]))
    return probs


# ---------------------------------------------------------------- test groups
def t_files(c):
    for rel in FROZEN_FILES:
        if c.exists(rel):
            c.rep.add("PASS", "files.exist", rel)
        else:
            c.rep.add("SKIP" if c.args.allow_missing else "FAIL", "files.exist", "missing " + rel)
    for s in SKILLS:
        rel = "skills/%s/SKILL.md" % s
        if not c.exists(rel):
            continue
    extra = sorted(p.name for p in (c.root / "skills").glob("*") if p.is_dir()) if (c.root / "skills").is_dir() else []
    c.rep.check("files.skill-set", set(extra) <= set(SKILLS) or not extra,
                "unexpected skill directories: %s" % sorted(set(extra) - set(SKILLS)), "WARN")


def t_manifests(c):
    r = c.rep
    claude = c.json(".claude-plugin/plugin.json", "manifest.claude")
    market = c.json(".claude-plugin/marketplace.json", "manifest.marketplace")
    codex = c.json(".codex-plugin/plugin.json", "manifest.codex")
    root = c.json("plugin.json", "manifest.root")
    versions = {}
    forbidden_keys = {"mcpServers", "permissions", "pricing", "price", "licence",
                      "userConfig", "mcp_servers"}
    for label, m in (("claude", claude), ("codex", codex), ("root", root)):
        if m is None:
            continue
        r.check("manifest.%s.name" % label, m.get("name") == "irrigator", "name must be 'irrigator'")
        v = m.get("version")
        r.check("manifest.%s.version" % label, isinstance(v, str) and bool(SEMVER.match(v)), "version semver: %r" % (v,))
        if isinstance(v, str):
            versions[label] = v
        r.check("manifest.%s.description" % label, isinstance(m.get("description"), str) and len(m["description"]) >= 20,
                "description present")
    if market is not None:
        r.check("manifest.marketplace.owner", isinstance(market.get("owner"), dict) and bool(market["owner"].get("name")),
                "owner.name present")
        entry = next((p for p in market.get("plugins", []) if isinstance(p, dict) and p.get("name") == "irrigator"), None)
        if r.check("manifest.marketplace.entry", entry is not None, "plugins[] has an 'irrigator' entry") and entry:
            if "version" in entry:
                versions["marketplace"] = entry["version"]
            src = entry.get("source")
            if isinstance(src, str):
                r.check("manifest.marketplace.source", src.startswith("./"), "string source must be a local ./ path: %r" % src)
            elif isinstance(src, dict):
                url = json.dumps(src)
                r.check("manifest.marketplace.source-remote", False,
                        "remote marketplace source %s is unverified; local-only parity expects './'" % url, "WARN")
            else:
                r.check("manifest.marketplace.source", False, "entry needs a source")
    if len(versions) > 1:
        r.check("manifest.versions-agree", len(set(versions.values())) == 1, "versions must agree: %s" % versions)
    if codex is not None:
        r.check("manifest.codex.skills", codex.get("skills") == "./skills/", ".codex-plugin skills must be './skills/'")
        iface = codex.get("interface")
        if r.check("manifest.codex.interface", isinstance(iface, dict), "interface object present", "WARN") and iface:
            r.check("manifest.codex.displayName", iface.get("displayName") == "Irrigator", "interface.displayName == 'Irrigator'", "WARN")
            dp = iface.get("defaultPrompt", [])
            r.check("manifest.codex.defaultPrompt", isinstance(dp, list) and all(isinstance(x, str) for x in dp),
                    "defaultPrompt is a list of strings")
    if claude is not None and "skills" in claude:
        r.check("manifest.claude.skills", (c.root / claude["skills"]).is_dir(), "skills path resolves from plugin root")
    # shared claims / path hygiene
    for label, m in (("claude", claude), ("market", market), ("codex", codex), ("root", root)):
        if m is None:
            continue
        for p, k, v in walk_json(m):
            if k == "license":
                r.check("claims.manifest-license", v == "MIT" and mit_license(c.root), "%s: license must match project MIT notice" % label)
            if k == "mcpServers" and label == "claude" and p == "/mcpServers" and mcp_backed(c.root):
                servers = v if isinstance(v, dict) else {}
                r.check("claims.manifest-mcp-entry", set(servers) == {"irrigator"} and mcp_server_entry_ok(servers["irrigator"]),
                        "claude: mcpServers may only register the read-only `irrigator mcp` server (no --allow-control)")
                continue
            if p.startswith("/mcpServers/") and label == "claude" and mcp_backed(c.root):
                continue
            if k in forbidden_keys:
                r.check("claims.manifest-key", False, "%s: key %s is forbidden (no MCP/permissions/pricing/license claims)" % (label, p))
            if isinstance(v, str):
                if GRAINULATOR_RE.search(v):
                    r.check("claims.manifest-grainulator", False, "%s: %s mentions Grainulator" % (label, p))
                if "mcp__" in v:
                    r.check("claims.manifest-mcp", False, "%s: %s references an MCP tool" % (label, p))
                if v.startswith("./") and ".." not in v and not (c.root / v).exists():
                    r.check("manifest.paths", False, "%s: %s -> %s does not exist (resolved from plugin root)" % (label, p, v))
                if ".." in v.split("/") and v.startswith("."):
                    r.check("manifest.paths", False, "%s: %s escapes plugin root" % (label, p))
    r.check("claims.no-mcp-file", not c.exists(".mcp.json"), "no .mcp.json (the MCP server is registered inline in the Claude manifest)")
    # Antigravity / Agent Plugins MCP files: the same read-only server, optionally typed stdio.
    for rel in ("mcp.json", "mcp_config.json"):
        if not c.exists(rel):
            continue
        try:
            servers = json.loads((c.root / rel).read_text(encoding="utf-8")).get("mcpServers")
        except (OSError, ValueError, AttributeError):
            servers = None
        entry = dict(servers.get("irrigator") or {}) if isinstance(servers, dict) else {}
        typed_ok = entry.pop("type", "stdio") == "stdio"
        r.check("claims.agent-plugin-mcp-entry",
                mcp_backed(c.root) and isinstance(servers, dict) and set(servers) == {"irrigator"} and typed_ok and mcp_server_entry_ok(entry),
                "%s: mcpServers may only register the read-only `irrigator mcp` server (stdio, no --allow-control)" % rel)
    r.check("claims.no-agents-dir", not (c.root / "agents").exists(), "no agents/ dir inherited from Grainulator", "WARN")


CONCEPTS = {
    "irrigator": [
        ("doctor", [r"irrigator doctor"]),
        ("public-metadata", [r"display", r"outcome_field"]),
        ("shape-vs-concurrency", [r"\bshape\b", r"concurrency|--workers"]),
        ("source-preflight", [r"pre-?flight"]),
        ("paid-authorization", [r"authori[sz]", r"budget"]),
        ("independent-acceptance", [r"independent", r"accept"]),
        ("routes-siblings", [r"plan", r"run", r"resume", r"observe", r"budget"]),
    ],
    "router": [
        ("routes-plan", [r"plan"]), ("routes-run", [r"\brun\b"]), ("routes-resume", [r"resume"]),
        ("routes-observe", [r"observe"]), ("routes-budget", [r"budget"]),
        ("when-not-to-route", [r"(when|do) not (to )?route|not route|do not use|when not"]),
        ("no-auto-paid", [r"authori[sz]"]),
    ],
    "plan": [
        ("api-version", [r"swarm/v1"]),
        ("display-fields", [r"why_spawned", r"done_when", r"outcome_field"]),
        ("display-limits", [r"\b48\b", r"\b100\b", r"\b60\b", r"\b140\b"]),
        ("shape-notation", [r"AxDxS|A×D×S|\b\d+x\d+x\d+\b"]),
        ("concurrency-separate", [r"--workers|concurrency", r"separate|not the same|independent of"]),
        ("source-preflight", [r"pre-?flight", r"source"]),
        ("immutable-plan", [r"immutable|preserved|pinned", r"propose"]),
        ("validate-cmd", [r"irrigator plan"]),
        ("size-cmd", [r"irrigator size"]),
    ],
    "run": [
        ("source-preflight", [r"pre-?flight", r"source"]),
        ("paid-authorization", [r"authori[sz]", r"paid"]),
        ("per-call-budget", [r"per[- ]call"]),
        ("workers", [r"--workers"]),
        ("mock-first", [r"--backend mock|mock"]),
        ("apply-cmd", [r"irrigator apply"]),
        ("dispatch-evidence", [r"dispatch", r"logs|inspect|status"]),
        ("independent-acceptance", [r"independent", r"accept"]),
        ("no-self-report", [r"self-report|worker (claim|report)|not trust|do not trust|verified"]),
    ],
    "resume": [
        ("status-first", [r"irrigator status"]),
        ("inspect", [r"irrigator inspect|irrigator logs"]),
        ("uncertain", [r"uncertain"]),
        ("resolve", [r"irrigator resolve", r"--result"]),
        ("allow-uncertain", [r"--allow-uncertain"]),
        ("no-blind-replay", [r"replay"]),
        ("retry-failed", [r"--retry-failed"]),
        ("resume-cmd", [r"irrigator resume"]),
        ("immutable-plan", [r"propose", r"immutable|original plan|pinned|preserved"]),
    ],
    "observe": [
        ("terminal-observation", [r"irrigator status", r"--watch"]),
        ("read-only", [r"read-only"]),
        ("status-json", [r"irrigator status"]),
        ("logs", [r"irrigator logs"]),
        ("inspect", [r"irrigator inspect"]),
        ("runtime-primary", [r"secondary|runtime[^.\n]*(primary|authoritative|source of truth)"]),
    ],
    "budget": [
        ("size-cmd", [r"irrigator size"]),
        ("per-call", [r"per[- ]call"]),
        ("not-graph-cap", [r"not (a )?(graph|total)|graph-wide|no graph"]),
        ("sizing-flag", [r"--sizing"]),
        ("authorization", [r"authori[sz]"]),
        ("estimates", [r"estimate|reported"]),
        ("shape-cmd", [r"--shape"]),
    ],
    "debug": [
        ("debug-cmd", [r"irrigator debug"]),
        ("read-only", [r"read-only"]),
        ("json-node", [r"--json", r"--node"]),
        ("irrigator-specific", [r"not a (universal|general)"]),
        ("fixed-codes", [r"error_code", r"never|not shown"]),
        ("uncertain-no-replay", [r"uncertain", r"replay"]),
        ("hands-to-resume", [r"resume skill|skills/resume"]),
        ("authorization", [r"authori[sz]"]),
        ("fallback", [r"status DIR --json", r"inspect"]),
        ("truncation", [r"omitted"]),
    ],
    # swarm: deliberately loose presence checks; the skill text is owned by another lane.
    "swarm": [
        ("cli-reference", [r"irrigator"]),
        ("authorization", [r"authori[sz]|budget"]),
        ("independent-acceptance", [r"independent|accept|verif"]),
    ],
}

SKILL_BANNED = [
    (GRAINULATOR_RE.pattern, "Grainulator vocabulary (the author name Grainulation is allowed)"),
    (r"mcp__", "MCP tool reference (Irrigator has no MCP server)"),
    (r"(?<![\w])/(research|witness|blind-spot|orchestrate|brief|present|challenge)\b", "Grainulator slash command"),
    (r"claims\.json|compilation\.json", "Grainulator ledger file"),
    (r"\bSkill tool\b|\bSkill\(", "Claude-only Skill tool assumption (Codex follows Markdown manually)"),
]


def t_skills(c):
    r = c.rep
    for s in SKILLS:
        rel = "skills/%s/SKILL.md" % s
        text = c.need(rel, "skill.%s" % s)
        if text is None:
            continue
        meta = parse_frontmatter(text)
        if not r.check("skill.%s.frontmatter" % s, meta is not None, "has YAML frontmatter"):
            continue
        r.check("skill.%s.name" % s, meta.get("name") == s, "frontmatter name == directory (%r)" % meta.get("name"))
        desc = meta.get("description")
        r.check("skill.%s.description" % s, isinstance(desc, str) and len(desc) >= 20, "description >= 20 chars")
        tools = meta.get("allowed-tools", [])
        tools = tools if isinstance(tools, list) else [tools]
        r.check("skill.%s.tools" % s, not any("mcp" in t.lower() for t in tools), "allowed-tools lists no MCP tools")
        r.check("skill.%s.length" % s, text.count("\n") <= 400, "<= 400 lines", "WARN")
        body = text
        for pat, label in SKILL_BANNED:
            for i, line in enumerate(body.splitlines(), 1):
                if re.search(pat, line, re.I if pat == GRAINULATOR_RE.pattern else 0) and not NEG.search(line):
                    r.check("skill.%s.banned" % s, False, "line %d: %s" % (i, label))
        for cid, pats in CONCEPTS[s]:
            ok = all(re.search(p, body, re.I | re.S) for p in pats)
            r.check("skill.%s.concept.%s" % (s, cid), ok, "must cover: " + " + ".join(pats))
    # cross-skill references use real skill names
    for rel in c.md_files():
        t = c.text(rel) or ""
        for m in re.finditer(r"/irrigator:([a-z-]+)", t):
            r.check("skill.xref", m.group(1) in SKILLS, "%s references /irrigator:%s" % (rel, m.group(1)))


def t_commands(c):
    r = c.rep
    n = 0
    for rel in c.md_files():
        t = c.text(rel) or ""
        for lineno, toks, origin in extract_invocations(t):
            n += 1
            # A narrowly marked design proposal is not an installed CLI contract.
            # Only the first inline example immediately after this explicit marker
            # is exempt; the same unsupported command elsewhere must still fail.
            prefix = "\n".join(t.splitlines()[:lineno - 1]).rstrip()
            if origin == "inline" and re.search(
                    r"Proposed bounded command, to be implemented separately and\s+reviewed:$", prefix):
                r.add("SKIP", "cli.proposed-example", "%s:%d explicitly unimplemented design proposal" % (rel, lineno))
                continue
            probs = validate_invocation(toks, c.command_flags, c.subcommands)
            if not probs:
                continue
            for lvl, msg in probs:
                r.add(lvl, "cli.invocation", "%s:%d (%s) `%s`: %s" % (rel, lineno, origin, " ".join(toks), msg))
            continue
    r.add("PASS", "cli.invocations-scanned", "%d documented irrigator invocations scanned against COMMAND_FLAGS" % n)
    # Legacy launch examples must choose a backend; a call budget is not backend selection.
    for rel in ("skills/run/SKILL.md", "skills/resume/SKILL.md"):
        t = c.text(rel)
        if not t:
            continue
        for lineno, toks, origin in extract_invocations(t):
            if len(toks) > 2 and toks[1] in ("apply", "prepare", "deliver"):
                has = any(x == "--backend" or x.startswith("--backend=") for x in toks)
                reference = origin == "inline" and toks[2] == "PLAN.json" and "--check" in toks and "--nodes" in toks
                if not has and not reference:
                    r.add("WARN", "cli.backend-selection", "%s:%d `%s` omits --backend; legacy launches require explicit selection (managed plans use admitted profiles)" % (rel, lineno, " ".join(toks)))


CLAIM_PATTERNS = [
    ("claims.remote-action", r"\bgit push\b|\bnpm publish\b|\bgh (release|repo create)\b|\bdocker push\b|\btwine upload\b|curl[^\n|]*\|\s*(ba)?sh",
     "remote/publishing action", True),
    ("claims.license", r"\bMIT License\b|\bApache[- ]2\.0\b|\bSPDX-License-Identifier\b|licensed under", "license claim", True),
    ("claims.pricing", r"\$\d+(\.\d+)?\s*/\s*(mo|month|seat|user|year)|\bfree tier\b|\bsubscription\b|\bpricing plan", "pricing claim", True),
    ("claims.publication", r"\bpublished (to|on|in)\b|available (on|in) the (marketplace|store|registry)|\bofficially (supported|certified|verified)\b|\bnow available\b|\bgenerally available\b",
     "public release / endorsement claim", True),
    ("claims.mcp", r"\bMCP server\b|\bmcpServers\b|mcp__", "MCP capability claim", True),
    ("claims.permissions", r"\bpermissions? (prompt|model|grant)s?\b|\bsandbox(ed)? (the )?(workers?|cells?)\b", "permission/sandbox claim", True),
    # Invented bypass claims; only a negation directly before the match clears them.
    ("claims.permission-bypass",
     r"\b(?:bypass\w*|circumvent\w*|auto-?approv\w*|pre-?approv\w*|disabl\w*|skip\w*|suppress\w*)\b[^.\n]{0,40}?\b(?:permission|approval|prompt|confirmation)s?\b"
     r"|\b(?:no|without|zero)\s+(?:host\s+)?(?:permission|approval)s?\s+(?:prompts?|checks?|needed|required)\b"
     r"|\bown\s+permissions?\s+(?:model|broker|system|layer|grants?)\b",
     "invented permission-bypass claim", "prefix"),
]


def t_claims(c):
    r = c.rep
    backed = mcp_backed(c.root)
    for rel in c.md_files():
        t = c.text(rel) or ""
        lines = t.splitlines()
        in_code = False
        for i, line in enumerate(lines, 1):
            if line.strip().startswith("```"):
                in_code = not in_code
            for tid, pat, label, negatable in CLAIM_PATTERNS:
                hit = next((m for m in re.finditer(pat, line, re.I)
                            if not claim_suppressed(tid, negatable, lines, i - 1, m, in_code, rel, backed)), None)
                if hit is not None:
                    if tid == "claims.license" and mit_license(c.root):
                        if re.search(r"\bMIT\b", line) and not re.search(r"Apache[- ]2\.0|\bGPL\b|SPDX-License-Identifier\s*:\s*(?!MIT\b)", line, re.I):
                            continue
                    r.add("FAIL", tid, "%s:%d %s: %s" % (rel, i, label, line.strip()[:100]))
        # graph-wide spending cap misstatement
        for i, line in enumerate(lines, 1):
            if re.search(r"--budget", line) and re.search(r"graph-wide|total (spend|cost)|overall (cap|limit)|hard cap", line, re.I) and not NEG.search(line):
                r.add("FAIL", "claims.budget-scope", "%s:%d --budget is per call, not a graph-wide cap" % (rel, i))
    r.add("PASS", "claims.scanned", "scanned %d markdown files for forbidden claims" % len(c.md_files()))


def t_notation(c):
    r = c.rep
    r.check("shape.math", shape_total(25, 5, 2) == 775 and shape_total(2, 5, 1) == 10 and shape_total(10, 5, 2) == 310,
            "A*sum(S^k) arithmetic: 25x5x2=775, 2x5x1=10, 10x5x2=310")
    for rel in c.md_files():
        t = c.text(rel) or ""
        for i, line in enumerate(t.splitlines(), 1):
            # Only explicit equalities assert a maximum. Layer populations, actual
            # execution counts and the runtime node ceiling are separate quantities.
            for a, d, branch, count in re.findall(r"(?<![\w-])(\d+)[x×](\d+)(?:[x×](\d+))?`?\s*=\s*\*{0,2}([\d,]+)", line):
                expected = shape_total(int(a), int(d), int(branch or 1))
                if int(count.replace(",", "")) != expected:
                    r.add("FAIL", "shape.line", "%s:%d explicit shape equality disagrees with maximum %d" % (rel, i, expected))
    notation = c.need("docs/runtime-notation.md", "notation.doc")
    if notation:
        for tid, pat in (("ownership", r"ownership"), ("workers-separate", r"--workers"),
                         ("example-775", r"25x5x2[^\n]*775|775[^\n]*25x5x2"), ("size-shape", r"irrigator size --shape"),
                         ("apply-needs-plan", r"apply[^\n]*plan"), ("depth-not-critical-path", r"critical path")):
            r.check("notation." + tid, bool(re.search(pat, notation, re.I)), "docs/runtime-notation.md covers " + pat)
    arch = c.need("docs/plugin-architecture.md", "arch.doc")
    if arch:
        runtime_primary = re.search(r"(?:runtime|CLI)[^.\n]*\b(?:primary|authoritative|source of truth)\b", arch, re.I)
        observer_secondary = re.search(r"\b(?:observer|panel|observe)\b[^.\n]*\bsecondary\b|\bsecondary\b[^.\n]*\b(?:observer|panel)\b", arch, re.I)
        r.check("arch.runtime-primary", bool(runtime_primary and observer_secondary),
                "architecture states runtime/CLI primary and observer secondary")
        mcp_lines = "".join(re.findall(r"[^\n]*MCP[^\n]*", arch))
        r.check("arch.no-mcp", bool(mcp_lines) and (bool(NEG.search(mcp_lines)) or (mcp_backed(c.root) and "mcp.md" in arch)),
                "architecture states its MCP posture (none, or the documented read-only server)")


def t_json_examples(c):
    r = c.rep
    checked = skipped = 0
    for rel in c.md_files():
        t = c.text(rel) or ""
        for m in FENCE.finditer(t):
            if m.group(1).lower() != "json":
                continue
            try:
                obj = json.loads(m.group(2))
            except ValueError:
                skipped += 1
                continue
            if isinstance(obj, dict) and ("tasks" in obj or "display" in obj):
                checked += 1
                line = t.count("\n", 0, m.start(2)) + 1
                for p in validate_display_plan(obj):
                    r.add("FAIL", "display.example", "%s:%d %s" % (rel, line, p))
                if "tasks" in obj:
                    r.check("display.example-api", obj.get("api_version") in (None, "swarm/v1", "swarm/v2", "swarm/v3", "swarm/v4", "swarm/v5", "swarm/v6"), "%s:%d unsupported plan api_version" % (rel, line))
    r.add("PASS", "display.examples-scanned", "%d plan/display JSON examples validated, %d non-JSON/elided blocks skipped" % (checked, skipped))
    # self-test of the validator so a broken validator cannot pass silently
    bad = {"tasks": [{"id": "x", "requires": ["a"], "file_fields": ["a"],
                      "display": {"title": "t" * 49, "outcome_field": "a"}}]}
    r.check("display.validator-selftest", len(validate_display_plan(bad)) == 2, "validator flags long title and file_fields outcome")
    good = {"display": {"version": 1, "title": "é" * 60, "purpose": "p"},
            "tasks": [{"id": "x", "requires": ["a"], "display": {"title": "é" * 48, "outcome_field": "a"}}]}
    r.check("display.validator-runes", validate_display_plan(good) == [], "limits count runes, not bytes")


PAGE_CHECKS = {
    "docs/codex-plugin.md": [(r"manual", "Codex follows the Markdown workflow manually"),
                             (r"\.codex-plugin", "documents .codex-plugin layout"),
                             (r"plugin\.json", "documents root plugin.json"),
                             (r"discovery|host.*lists|host.*install", "describes host discovery boundary"),
                             (r"MCP", "states MCP posture")],
    "docs/claude-plugin.md": [(r"--plugin-dir|/plugin install", "documents plugin loading"),
                              (r"local", "local install framing"),
                              (r"discovery|host.*lists|host.*install", "describes host discovery boundary"),
                              (r"MCP", "states MCP posture")],
    "docs/local-release.md": [(r"local", "local framing"),
                              (r"(no|not|never)[^\n]*(push|publish)", "states no push/publish")],
    "docs/license-inventory.md": [(r"license", "mentions licensing"),
                                  (r"MIT", "documents project MIT licensing")],
    "SECURITY.md": [(r"not (a )?sandbox|no sandbox", "cells are not sandboxes")],
    "CONTRIBUTING.md": [(r"local", "local workflow")],
    "README.md": [(r"plugin|docs/index\.md", "links plugin or documentation entrypoint"), (r"irrigator doctor", "documents doctor")],
}


def t_docs(c):
    r = c.rep
    for rel, checks in PAGE_CHECKS.items():
        t = c.need(rel, "docs." + rel)
        if t is None:
            continue
        for pat, label in checks:
            r.check("docs.%s" % rel, bool(re.search(pat, t, re.I | re.S)), label)
    readme = c.text("README.md")
    if readme and not re.search(r"docs/(?:index|claude-plugin|codex-plugin)\.md", readme):
        for s in SKILLS:
            r.check("docs.readme.skill.%s" % s, bool(re.search(r"\b%s\b" % s, readme)), "README mentions skill '%s'" % s, "WARN")


# ELF, Mach-O (32/64, both byte orders, fat/universal). PE is handled by name below.
NATIVE_MAGIC = tuple(bytes.fromhex(h) for h in ("7f454c46", "cffaedfe", "cefaedfe", "cafebabe", "feedfacf", "feedface"))
BAD_NAMES = {".env", "id_rsa", "id_ed25519", ".npmrc", ".netrc", "credentials.json"}
LICENSE_NAMES = {"LICENSE", "LICENSE.MD", "LICENSE.TXT", "COPYING"}
ARCHIVE_MAX_MEMBERS = 100000


def hygiene_findings(rel, head):
    """Pure per-file verdicts (name + first bytes), shared by the root and archive scans."""
    out = []
    name = rel.rsplit("/", 1)[-1]
    suffix = os.path.splitext(name)[1]
    if name in BAD_NAMES or suffix in (".pem", ".key", ".p12"):
        out.append(("FAIL", "tree.secrets", "credential-like file: " + rel))
    if name == ".DS_Store":
        out.append(("WARN", "tree.junk", rel))
    if name.upper() in LICENSE_NAMES:
        out.append(("PASS", "tree.license", "%s included; licensing claims are checked against the project notice" % rel))
    if head[:4] in NATIVE_MAGIC or (head[:2] == b"MZ" and suffix.lower() in (".exe", ".dll")):
        out.append(("FAIL", "tree.binary", "native executable bundled: " + rel))
    return out


def archive_name_problems(name):
    norm = name.replace(chr(92), "/")
    parts = [x for x in norm.split("/") if x]
    probs = []
    if norm.startswith("/") or re.match(r"^[A-Za-z]:", norm):
        probs.append("absolute path")
    if ".." in parts:
        probs.append("parent-directory traversal")
    if ".git" in parts:
        probs.append("version-control metadata")
    return probs


def scan_root(r, root):
    """Scan package files; recognized Git source checkouts prune generated build trees."""
    files = 0
    for p in source_files(root):
        rel = p.relative_to(root).as_posix()
        if rel.startswith((".git/",)) or "__pycache__" in rel.split("/"):
            continue
        if p.is_symlink():
            r.add("WARN", "tree.symlink", "symlink not followed: " + rel)
            continue
        if p.is_dir():
            continue
        files += 1
        try:
            with open(p, "rb") as f:
                head = f.read(4)
        except OSError as e:
            r.add("WARN", "tree.unreadable", "%s could not be read for the binary check: %s" % (rel, e))
            head = b""
        for lvl, tid, msg in hygiene_findings(rel, head):
            r.add(lvl, tid, msg)
    return files


def scan_archive(r, path):
    """Scan the real members of a release archive in place. Never extracts, never skips a member."""
    p = Path(path)
    if not p.is_file():
        r.add("FAIL", "archive.exists", "release archive is not a file: %s" % p)
        return
    seen = {"members": 0, "files": 0}

    def note(name):
        seen["members"] += 1
        if seen["members"] > ARCHIVE_MAX_MEMBERS:
            raise ValueError("more than %d members" % ARCHIVE_MAX_MEMBERS)
        for prob in archive_name_problems(name):
            r.add("FAIL", "tree.path", "archive member %r: %s" % (name, prob))

    def judge(name, head):
        seen["files"] += 1
        for lvl, tid, msg in hygiene_findings(name.rstrip("/"), head):
            r.add(lvl, tid, "archive member: " + msg)

    try:
        if zipfile.is_zipfile(str(p)):
            with zipfile.ZipFile(str(p)) as z:
                for info in z.infolist():
                    note(info.filename)
                    if info.is_dir():
                        continue
                    with z.open(info) as f:
                        judge(info.filename, f.read(4))
        else:
            with tarfile.open(str(p), "r:*") as t:
                for m in t:
                    note(m.name)
                    if m.isdir():
                        continue
                    if m.issym() or m.islnk():
                        probs = archive_name_problems(m.linkname)
                        r.add("FAIL" if probs else "WARN", "tree.link", "archive member %r links to %r%s" % (
                            m.name, m.linkname, " (" + ", ".join(probs) + ")" if probs else ""))
                        continue
                    if not m.isreg():
                        r.add("FAIL", "tree.special", "archive member %r is a device/fifo/special entry" % m.name)
                        continue
                    f = t.extractfile(m)
                    judge(m.name, f.read(4) if f is not None else b"")
    except Exception as e:  # noqa: BLE001 - an unreadable archive must fail, never pass
        r.add("FAIL", "archive.read", "cannot scan %s: %s" % (p, e))
        return
    if seen["members"] == 0:
        r.add("FAIL", "archive.empty", "release archive has no members: %s" % p)
    else:
        r.add("PASS", "archive.scanned", "scanned %d members (%d files) of %s" % (seen["members"], seen["files"], p))


def t_tree(c):
    r = c.rep
    archive = getattr(c.args, "archive", None)
    if archive:
        scan_archive(r, archive)
        r.add("PASS", "tree.scope", "hygiene scope is the members of archive %s; --root %s was not binary-scanned" % (archive, c.root))
        return
    if not getattr(c.args, "root_explicit", True):
        r.add("WARN", "tree.scope", "no --root or --archive given: scanned the default checkout %s, which is not release "
              "content; pass --archive RELEASE or --root PACKAGE_ROOT for acceptance" % c.root)
    files = scan_root(r, c.root)
    r.add("PASS", "tree.scanned", "tree hygiene scan complete: %d files under %s" % (files, c.root))


def t_package_tool(c):
    r = c.rep
    rel = "tools/package-plugin.py"
    src = c.need(rel, "package.tool")
    if src is None:
        return
    try:
        compile(src, rel, "exec")
        r.add("PASS", "package.syntax", "compiles")
    except SyntaxError as e:
        r.add("FAIL", "package.syntax", str(e))
    net = re.findall(r"^\s*(?:import|from)\s+(urllib|http|socket|requests|ftplib|smtplib|ssl|xmlrpc|webbrowser)\b", src, re.M)
    r.check("package.no-network", not net, "no network imports: %s" % sorted(set(net)))
    r.check("package.no-remote-cmds", not re.search(r"[\"']push[\"']|[\"']gh[\"']|[\"']publish[\"']|[\"']upload[\"']", src),
            "no git push / gh / publish / upload subprocess usage")
    r.check("package.argparse", "argparse" in src, "takes explicit path arguments", "WARN")
    r.check("package.no-install", not re.search(r"\.claude/plugins|\.codex/plugins|expanduser", src),
            "does not write into host plugin directories", "WARN")


# ---------------------------------------------------------------- binary tests
def run_bin(binary, args, cwd, timeout=60):
    env = dict(os.environ)
    env["IRRIGATOR_UI"] = "plain"
    env["NO_COLOR"] = "1"
    try:
        p = subprocess.run([binary] + args, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
    except Exception as e:  # noqa: BLE001 - report any launch failure
        return None, "", str(e)
    return p.returncode, p.stdout, p.stderr


def t_binary(c):
    r = c.rep
    b = os.path.abspath(c.args.binary)
    if not (os.path.isfile(b) and os.access(b, os.X_OK)):
        r.add("FAIL", "binary.exists", "not an executable file: " + b)
        return
    with tempfile.TemporaryDirectory(prefix="irrigator-contract-") as cwd:
        rc, out, err = run_bin(b, ["version"], cwd)
        r.check("binary.version", rc == 0 and bool(out.strip()), "version prints: %r" % out.strip())
        rc, out, err = run_bin(b, ["--help"], cwd)
        r.check("binary.help", rc == 0, "--help exits 0")
        missing = [cmd for cmd in HELP_COMMANDS if cmd not in ("help",) and not re.search(r"\b%s\b" % re.escape(cmd), out)]
        r.check("binary.help-commands", not missing, "COMMAND_FLAGS commands absent from --help: %s" % missing)
        used = set()
        for rel in c.md_files():
            for _, toks, _o in extract_invocations(c.text(rel) or ""):
                if len(toks) > 1 and toks[1] in COMMAND_FLAGS:
                    used.add(toks[1])
        gone = sorted(x for x in used if x != "help" and x not in HIDDEN_ALIASES
                      and not re.search(r"\b%s\b" % re.escape(x), out))
        r.check("binary.docs-commands-exist", not gone, "documented commands missing from this binary: %s" % gone)
        rc, out, err = run_bin(b, ["definitely-not-a-command"], cwd)
        r.check("binary.unknown-command", rc == 2, "unknown command exits 2 (got %s)" % rc)
        rc, out, err = run_bin(b, ["doctor"], cwd)
        try:
            rep = json.loads(out)
            r.check("binary.doctor", isinstance(rep, dict) and "irrigator_version" in rep, "doctor prints JSON with irrigator_version")
        except ValueError:
            r.add("WARN", "binary.doctor", "doctor stdout is not JSON (rc=%s)" % rc)
    if c.args.smoke:
        smoke(c, b)


def smoke(c, b):
    """Credential-free mock run in a fresh temp dir. Never writes under --root."""
    r = c.rep
    with tempfile.TemporaryDirectory(prefix="irrigator-smoke-") as cwd:
        plan = os.path.join(cwd, "plan.json")
        run = os.path.join(cwd, "runs", "first")
        reg = os.path.join(cwd, "session.json")
        rc, out, err = run_bin(b, ["init", "--out", plan], cwd)
        if not r.check("smoke.init", rc == 0 and os.path.isfile(plan), "init writes plan (rc=%s) %s" % (rc, err.strip()[:120])):
            return
        rc, out, err = run_bin(b, ["plan", plan, "--json"], cwd)
        r.check("smoke.plan", rc == 0, "plan --json validates starter plan")
        rc, out, err = run_bin(b, ["apply", plan, "--run", run, "--backend", "mock", "--workers", "2",
                                   "--session-registry", reg], cwd, 120)
        r.check("smoke.apply-mock", rc == 0, "apply --backend mock completes (rc=%s) %s" % (rc, err.strip()[:120]))
        rc, out, err = run_bin(b, ["status", run, "--json"], cwd)
        try:
            st = json.loads(out)
            r.check("smoke.status-json", isinstance(st, dict), "status --json parses")
        except ValueError:
            r.add("FAIL", "smoke.status-json", "status --json did not print JSON")
        rc, out, err = run_bin(b, ["logs", run, "--json", "--since", "0"], cwd)
        lines = [l for l in out.splitlines() if l.strip()]
        ok = rc == 0 and bool(lines)
        for l in lines:
            try:
                json.loads(l)
            except ValueError:
                ok = False
        r.check("smoke.logs-jsonl", ok, "logs --json is non-empty JSONL")
        rc, out, err = run_bin(b, ["inspect", run, "draft"], cwd)
        r.check("smoke.inspect", rc == 0, "inspect RUN draft succeeds")
        rc, out, err = run_bin(b, ["graph", run], cwd)
        r.check("smoke.graph", rc == 0 and bool(out.strip()), "graph prints Mermaid text")
        rc, out, err = run_bin(b, ["events", run], cwd)
        try:
            ev = json.loads(out)
            r.check("smoke.events", rc == 0 and isinstance(ev, list) and bool(ev), "events RUN prints the JSON event array (main.go alias)")
        except ValueError:
            r.add("FAIL", "smoke.events", "events RUN did not print JSON")
        rc, out, err = run_bin(b, ["artifacts", run], cwd)
        try:
            art_map = json.loads(out)
            r.check("smoke.artifacts", rc == 0 and isinstance(art_map, dict), "artifacts RUN prints a JSON map of verified results (main.go alias)")
        except ValueError:
            r.add("FAIL", "smoke.artifacts", "artifacts RUN did not print JSON")
        art = os.path.join(cwd, "artifacts")
        rc, out, err = run_bin(b, ["export", run, "--dir", art], cwd)
        r.check("smoke.export", rc == 0 and os.path.isdir(art), "export --dir writes artifacts (new directory)")
        rc, out, err = run_bin(b, ["export", run, "--dir", art], cwd)
        r.check("smoke.export-no-overwrite", rc != 0, "export refuses an existing directory")
        rc, out, err = run_bin(b, ["apply", plan, "--run", run, "--backend", "mock"], cwd)
        r.check("smoke.no-overwrite-run", rc != 0, "apply refuses an existing run directory")
        # display metadata acceptance / rejection
        good = {"api_version": "swarm/v1", "name": "contract",
                "display": {"version": 1, "publish": False, "title": "Contract", "purpose": "Plugin contract smoke"},
                "tasks": [{"id": "a", "prompt": "Say ok", "requires": ["out"], "outputs": {"out": "out.txt"},
                           "display": {"title": "A", "why_spawned": "root", "done_when": "out exists", "outcome_field": "out"}}]}
        bad_title = json.loads(json.dumps(good))
        bad_title["tasks"][0]["display"]["title"] = "t" * 49
        bad_field = json.loads(json.dumps(good))
        bad_field["tasks"][0]["display"]["outcome_field"] = "missing"
        bad_ver = json.loads(json.dumps(good))
        bad_ver["display"]["version"] = 2
        for name, obj, want_ok in (("good", good, True), ("bad-title", bad_title, False),
                                   ("bad-outcome-field", bad_field, False), ("bad-plan-version", bad_ver, False)):
            pth = os.path.join(cwd, "display-%s.json" % name)
            with open(pth, "w", encoding="utf-8") as f:
                json.dump(obj, f)
            rc, out, err = run_bin(b, ["plan", pth], cwd)
            r.check("smoke.display.%s" % name, (rc == 0) == want_ok,
                    "plan %s display metadata %s (rc=%s) %s" % (name, "accepted" if want_ok else "rejected", rc, err.strip()[:100]))
        rc, out, err = run_bin(b, ["size", "--shape", "25x5x2", "--json"], cwd)
        if rc == 0:
            r.check("smoke.size-shape", "775" in out, "size --shape 25x5x2 reports 775 maximum nodes")
        else:
            level = "SKIP" if "--sizing is required" in err else "WARN"
            r.add(level, "smoke.size-shape", "size --shape without --sizing exited %s (it needs a sizing file, which this smoke does not fabricate): %s" % (rc, err.strip()[:100]))
        tool = c.path("tools/package-plugin.py")
        if tool.is_file():
            try:
                p = subprocess.run([sys.executable, "-I", str(tool), "--help"], cwd=cwd, capture_output=True, text=True, timeout=30)
                r.check("smoke.package-help", p.returncode == 0, "package-plugin.py --help exits 0", "WARN")
            except Exception as e:  # noqa: BLE001
                r.add("WARN", "smoke.package-help", str(e))


GROUPS = [t_files, t_manifests, t_skills, t_commands, t_claims, t_notation, t_json_examples, t_docs, t_tree, t_package_tool]


def main(argv=None):
    ap = argparse.ArgumentParser(description="Irrigator plugin contract test (local, read-only)")
    ap.add_argument("--root", default=None, help="package root to scan (default: parent of verification/, reported as a WARN scope because a checkout is not release content)")
    ap.add_argument("--archive", help="release archive: static checks use bounded extraction unless --root is explicit; hygiene always scans every archive member")
    ap.add_argument("--json", action="store_true", help="machine-readable report")
    ap.add_argument("--strict", action="store_true", help="WARN also fails")
    ap.add_argument("--allow-missing", action="store_true", help="missing sibling-lane files are SKIP, not FAIL")
    ap.add_argument("--binary", help="absolute path to a built irrigator executable")
    ap.add_argument("--smoke", action="store_true", help="mock-backend smoke run in a temp dir (needs --binary)")
    args = ap.parse_args(argv)
    if args.smoke and not args.binary:
        print("--smoke requires --binary", file=sys.stderr)
        return 2
    if args.archive and args.root is None:
        spec = importlib.util.spec_from_file_location("consumer_extract", Path(__file__).with_name("plugin-consumer-test.py"))
        consumer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(consumer)
        with tempfile.TemporaryDirectory(prefix="irrigator-contract-") as tmp:
            try:
                extracted = consumer.extract_archive(Path(args.archive).resolve(), Path(tmp))
            except (ValueError, OSError) as exc:
                print("invalid archive: %s" % exc, file=sys.stderr)
                return 1
            forwarded = list(argv if argv is not None else sys.argv[1:])
            return main(forwarded + ["--root", str(extracted)])
    args.root_explicit = args.root is not None
    root = Path(args.root if args.root is not None else Path(__file__).resolve().parent.parent).resolve()
    if not root.is_dir():
        print("root is not a directory: %s" % root, file=sys.stderr)
        return 2
    rep = Reporter()
    ctx = Ctx(root, args, rep)
    if args.binary:
        with tempfile.TemporaryDirectory(prefix="irrigator-help-") as cwd:
            rc, out, err = run_bin(os.path.abspath(args.binary), ["help", "--json"], cwd)
        try:
            if rc != 0:
                raise ValueError("help --json failed: " + err[:200])
            live_flags, live_subcommands, live_hidden = parse_cli_contract(json.loads(out))
            ctx.command_flags, ctx.subcommands = live_flags, live_subcommands
            rep.check("binary.command-contract", (live_flags, live_subcommands, live_hidden) == (COMMAND_FLAGS, SUBCOMMANDS, HIDDEN_ALIASES), "generated CLI snapshot matches supplied executable")
        except (ValueError, TypeError, AttributeError) as exc:
            rep.add("FAIL", "binary.command-contract", str(exc))
    groups = list(GROUPS)
    if args.binary:
        groups.append(t_binary)
    for g in groups:
        try:
            g(ctx)
        except Exception as e:  # noqa: BLE001 - a crashing group is a failing group
            rep.add("FAIL", g.__name__, "test group crashed: %r" % (e,))
    if args.json:
        print(json.dumps({"root": str(root), "scope": SCOPE_NOTE, "summary": {k: rep.count(k) for k in ("PASS", "WARN", "FAIL", "SKIP")},
                          "results": rep.rows}, indent=2))
    else:
        for row in rep.rows:
            if row["level"] != "PASS":
                print("[%s] %s: %s" % (row["level"], row["id"], row["message"]))
        print("\nscope: %s" % SCOPE_NOTE)
        print("\nsummary: %d pass, %d warn, %d fail, %d skip (root %s)" % (
            rep.count("PASS"), rep.count("WARN"), rep.count("FAIL"), rep.count("SKIP"), root))
    if rep.count("FAIL") or (args.strict and rep.count("WARN")):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
