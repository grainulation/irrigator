#!/usr/bin/env python3
"""Native regression tests for verification/plugin-contract-test.py.

Run (stdlib only, local only):

    python3 -I verification/plugin-validator-regression.py [-v]

Every test builds its fixtures in a fresh temporary directory. Nothing is written
into the plugin tree, installed into a host, pushed, published or sent over a
network. Optional: set IRRIGATOR_MAIN_GO=/abs/path/internal/cli/dispatch.go to also check the
command aliases against the real CLI source.

Protected corrections: the legitimate author "Grainulation" is not a Grainulator
mention; main.go's hidden `events`/`artifacts` aliases are accepted; honest
host-permission disclaimers pass while invented bypass claims still fail; release
hygiene is scoped to explicit archive members or an explicit package root and
real native-binary contamination always fails.

"""
import argparse
import contextlib
import importlib.util
import io
import json
import os
import re
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
_SPEC = importlib.util.spec_from_file_location("plugin_contract_test", str(HERE / "plugin-contract-test.py"))
V = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(V)

ELF = bytes.fromhex("7f454c46") + bytes(12)
MACHO = bytes.fromhex("cffaedfe") + bytes(12)
TEXT = b"plain text\n"


def make_ctx(root, **over):
    ns = argparse.Namespace(allow_missing=True, archive=None, root_explicit=True, binary=None,
                            smoke=False, strict=False, json=False)
    for key, val in over.items():
        setattr(ns, key, val)
    rep = V.Reporter()
    return V.Ctx(Path(root), ns, rep), rep


def rows(rep, level, prefix=""):
    return [r for r in rep.rows if r["level"] == level and r["id"].startswith(prefix)]


def put(root, rel, data):
    p = Path(root) / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(data, str):
        p.write_text(data, encoding="utf-8")
    else:
        p.write_bytes(data)
    return p


def make_tar(path, members):
    with tarfile.open(str(path), "w:gz") as t:
        for name, data in members.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            t.addfile(info, io.BytesIO(data))


def make_zip(path, members):
    with zipfile.ZipFile(str(path), "w") as z:
        for name, data in members.items():
            z.writestr(name, data)


def run_main(argv):
    buf = io.StringIO()
    with contextlib.redirect_stdout(buf):
        rc = V.main(argv)
    return rc, buf.getvalue()


class GrainulatorVocabulary(unittest.TestCase):
    def test_author_name_is_not_the_product(self):
        self.assertIsNone(V.GRAINULATOR_RE.search("Grainulation"))
        self.assertIsNone(V.GRAINULATOR_RE.search("Made by GRAINULATION contributors"))
        for text in ("Grainulator", "grainulator plan", "Grainulators", "the grainulate step"):
            self.assertIsNotNone(V.GRAINULATOR_RE.search(text), text)

    def manifest_fails(self, author):
        with tempfile.TemporaryDirectory() as d:
            put(d, "plugin.json", json.dumps({
                "name": "irrigator", "version": "1.0.0-rc.4",
                "description": "Local Irrigator workflow skills over the irrigator CLI",
                "author": {"name": author}, "interface": {"developerName": author}}))
            c, rep = make_ctx(d)
            V.t_manifests(c)
            return rows(rep, "FAIL", "claims.manifest-grainulator")

    def test_manifest_author_grainulation_passes(self):
        self.assertEqual(self.manifest_fails("Grainulation"), [])

    def test_manifest_grainulator_still_fails(self):
        self.assertEqual(len(self.manifest_fails("Grainulator")), 2)

    def test_skill_ban_keeps_product_vocabulary_only(self):
        pat = V.SKILL_BANNED[0][0]
        self.assertIsNone(re.search(pat, "Authored by Grainulation", re.I))
        self.assertIsNotNone(re.search(pat, "Import the Grainulator claims", re.I))


class CliAliases(unittest.TestCase):
    def check(self, line):
        toks = V.parse_invocation(line)
        self.assertIsNotNone(toks, line)
        return V.validate_invocation(toks)

    def test_events_and_artifacts_accepted(self):
        self.assertEqual(self.check("irrigator events DIR"), [])
        self.assertEqual(self.check("irrigator artifacts runs/first"), [])

    def test_inline_documentation_extraction(self):
        text = "Use `irrigator events DIR` or `irrigator artifacts DIR` for JSON.\n"
        found = V.extract_invocations(text)
        self.assertEqual(len(found), 2)
        for _line, toks, _origin in found:
            self.assertEqual(V.validate_invocation(toks), [])

    def test_aliases_do_not_loosen_validation(self):
        for line in ("irrigator events DIR --follow", "irrigator frobnicate DIR", "irrigator _worker a b"):
            self.assertTrue(any(lvl == "FAIL" for lvl, _msg in self.check(line)), line)

    def test_aliases_are_not_expected_in_help(self):
        self.assertEqual(V.HIDDEN_ALIASES, {"events", "artifacts"})
        for name in V.HIDDEN_ALIASES:
            self.assertIn(name, V.COMMAND_FLAGS)
            self.assertNotIn(name, V.HELP_COMMANDS)

    @unittest.skipUnless("IRRIGATOR_MAIN_GO" in os.environ or (ROOT / "go.mod").is_file(), "no Go source in this package (packaged bundle)")
    def test_aliases_match_main_go(self):
        path = Path(os.environ.get("IRRIGATOR_MAIN_GO", ROOT / "internal/cli/dispatch.go"))
        # A source checkout must contain the CLI dispatch file; a missing file is a failure, not a skip.
        self.assertTrue(path.is_file(), f"CLI dispatch source missing: {path}")
        src = path.read_text(encoding="utf-8")
        self.assertIsNotNone(re.search(r'case "graph", "events", "artifacts"', src))


class PermissionWording(unittest.TestCase):
    def permission_fails(self, text):
        with tempfile.TemporaryDirectory() as d:
            put(d, "docs/page.md", text)
            c, rep = make_ctx(d)
            V.t_claims(c)
            return rows(rep, "FAIL", "claims.permission")

    def test_honest_host_disclaimers_pass(self):
        honest = (
            "Exact syntax can vary. consult help if a step is\nrejected. Host permission prompts apply: approve shell commands such as\n"
            "`irrigator apply` deliberately, and do not pre-approve paid execution broadly.\n",
            "2. Follow `docs/claude-plugin.md` for host-specific local loading. Your host's permission prompts apply.\n",
            "Use the host shell for commands. The host's permission prompts apply and a denied command is not retried.\n",
            "Irrigator never bypasses host permission prompts.\n",
            "Installing uses the host’s own permission model.\n",
            "Installing uses the host's own permission model.\n",
        )
        for text in honest:
            self.assertEqual(self.permission_fails(text), [], text)

    def test_invented_claims_fail(self):
        invented = (
            "The plugin bypasses host permission prompts.\n",
            "Irrigator has its own permission model.\n",
            "The host’s own permission model applies, but the plugin bypasses approval prompts.\n",
            "No permission prompts are needed.\n",
            "Host permission prompts apply, but this plugin skips approval for paid runs.\n",
            "Irrigator pre-approves every shell command, skipping permission checks.\n",
            "Irrigator can sandbox the workers.\n",
        )
        for text in invented:
            self.assertTrue(self.permission_fails(text), text)

    def test_list_items_do_not_bless_each_other(self):
        text = "- Host permission prompts apply.\n- The plugin has its own permission model.\n"
        self.assertTrue(self.permission_fails(text))

    @unittest.skipUnless((ROOT / "docs" / "claude-plugin.md").is_file(), "no docs in this checkout")
    def test_repository_docs_have_no_permission_failures(self):
        c, rep = make_ctx(ROOT)
        V.t_claims(c)
        self.assertEqual(rows(rep, "FAIL", "claims.permission"), [])


OLD_ARCH = "# Architecture\n\nThe CLI is the source of truth. There is no MCP server.\n"
NEW_ARCH = OLD_ARCH + ("The runtime (the `irrigator` CLI and its durable run files) is primary. "
                       "The observer (panel and the observe skill) is secondary and read-only.\n")


class DocumentationWording(unittest.TestCase):
    def arch_fails(self, text):
        with tempfile.TemporaryDirectory() as d:
            put(d, "docs/plugin-architecture.md", text)
            c, rep = make_ctx(d)
            V.t_notation(c)
            return rows(rep, "FAIL", "arch.runtime-primary")

    def test_runtime_primary_requires_observer_secondary(self):
        self.assertTrue(self.arch_fails(OLD_ARCH))
        self.assertEqual(self.arch_fails(NEW_ARCH), [])

    def claude_fails(self, text):
        with tempfile.TemporaryDirectory() as d:
            put(d, "docs/claude-plugin.md", text)
            c, rep = make_ctx(d)
            V.t_docs(c)
            return rows(rep, "FAIL", "docs.docs/claude-plugin.md")

    def test_claude_page_describes_loading_and_discovery(self):
        base = "Layout: `.claude-plugin`. Local only. MCP: none.\n"
        self.assertTrue(self.claude_fails(base))
        self.assertEqual(self.claude_fails(base + "Load with --plugin-dir after host discovery.\n"), [])

    @unittest.skipUnless((ROOT / "docs" / "plugin-architecture.md").is_file(), "no docs in this checkout")
    def test_repository_docs_pass_page_checks(self):
        c, rep = make_ctx(ROOT)
        V.t_notation(c)
        V.t_docs(c)
        self.assertEqual(rows(rep, "FAIL", "arch."), [])
        self.assertEqual(rows(rep, "FAIL", "docs.docs/claude-plugin.md"), [])


class ReleaseScope(unittest.TestCase):
    def scan(self, root_files=None, archive_members=None, kind="tgz", explicit=True):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d) / "root"
            root.mkdir()
            for rel, data in (root_files or {}).items():
                put(root, rel, data)
            archive = None
            if archive_members is not None:
                archive = str(Path(d) / ("rel.zip" if kind == "zip" else "rel.tar.gz"))
                (make_zip if kind == "zip" else make_tar)(archive, archive_members)
            c, rep = make_ctx(root, archive=archive, root_explicit=explicit)
            V.t_tree(c)
            return rep

    def test_hygiene_findings(self):
        self.assertTrue(any(t == "tree.binary" for _l, t, _m in V.hygiene_findings("bin/irrigator", ELF)))
        self.assertTrue(any(t == "tree.binary" for _l, t, _m in V.hygiene_findings("bin/irrigator", MACHO)))
        self.assertTrue(any(t == "tree.binary" for _l, t, _m in V.hygiene_findings("x.exe", b"MZ\x90\x00")))
        self.assertEqual(V.hygiene_findings("notes.txt", b"MZ\x90\x00"), [])
        self.assertTrue(any(t == "tree.license" for _l, t, _m in V.hygiene_findings("LICENSE.md", b"abcd")))
        self.assertTrue(any(t == "tree.secrets" for _l, t, _m in V.hygiene_findings("a/.env", b"abcd")))

    def test_root_binary_contamination_fails(self):
        for blob in (ELF, MACHO):
            rep = self.scan(root_files={"README.md": TEXT, "bin/irrigator": blob})
            self.assertTrue(rows(rep, "FAIL", "tree.binary"))

    def test_clean_explicit_root_passes(self):
        rep = self.scan(root_files={"README.md": TEXT})
        self.assertEqual(rows(rep, "FAIL"), [])
        self.assertTrue(rows(rep, "PASS", "tree.scanned"))
        self.assertEqual(rows(rep, "WARN", "tree.scope"), [])

    def test_default_root_warns_but_never_blesses_binaries(self):
        rep = self.scan(root_files={"irrigator": ELF}, explicit=False)
        self.assertTrue(rows(rep, "WARN", "tree.scope"))
        self.assertTrue(rows(rep, "FAIL", "tree.binary"))

    def test_clean_archive_passes(self):
        for kind in ("tgz", "zip"):
            rep = self.scan(archive_members={"irrigator/README.md": TEXT}, kind=kind)
            self.assertEqual(rows(rep, "FAIL"), [], kind)
            self.assertTrue(rows(rep, "PASS", "archive.scanned"), kind)

    def test_archive_binary_member_fails(self):
        for kind in ("tgz", "zip"):
            rep = self.scan(archive_members={"irrigator/README.md": TEXT, "irrigator/bin/irrigator": ELF}, kind=kind)
            self.assertTrue(rows(rep, "FAIL", "tree.binary"), kind)

    def test_archive_scope_replaces_root_scan_visibly(self):
        rep = self.scan(root_files={"irrigator": ELF}, archive_members={"irrigator/README.md": TEXT})
        self.assertEqual(rows(rep, "FAIL"), [])
        scope = rows(rep, "PASS", "tree.scope")
        self.assertTrue(scope and "archive" in scope[0]["message"])
        rep = self.scan(root_files={"README.md": TEXT}, archive_members={"irrigator/irrigator": MACHO})
        self.assertTrue(rows(rep, "FAIL", "tree.binary"))

    def test_archive_path_secret_and_vcs_members_fail(self):
        for name in ("../evil.txt", "/abs/evil.txt", "irrigator/.git/config"):
            rep = self.scan(archive_members={name: TEXT})
            self.assertTrue(rows(rep, "FAIL", "tree.path"), name)
        rep = self.scan(archive_members={"irrigator/.env": TEXT})
        self.assertTrue(rows(rep, "FAIL", "tree.secrets"))

    def test_archive_escaping_symlink_fails(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "rel.tar.gz"
            with tarfile.open(str(path), "w:gz") as t:
                info = tarfile.TarInfo("irrigator/link")
                info.type = tarfile.SYMTYPE
                info.linkname = "../../etc/passwd"
                t.addfile(info)
            c, rep = make_ctx(d, archive=str(path))
            V.t_tree(c)
            self.assertTrue(rows(rep, "FAIL", "tree.link"))

    def test_unreadable_missing_and_empty_archives_fail(self):
        with tempfile.TemporaryDirectory() as d:
            junk = put(d, "junk.tar.gz", b"this is not an archive")
            c, rep = make_ctx(d, archive=str(junk))
            V.t_tree(c)
            self.assertTrue(rows(rep, "FAIL", "archive.read"))
            c, rep = make_ctx(d, archive=str(Path(d) / "missing.tar.gz"))
            V.t_tree(c)
            self.assertTrue(rows(rep, "FAIL", "archive.exists"))
            empty = Path(d) / "empty.tar.gz"
            make_tar(empty, {})
            c, rep = make_ctx(d, archive=str(empty))
            V.t_tree(c)
            self.assertTrue(rows(rep, "FAIL", "archive.empty"))


class MainWiring(unittest.TestCase):
    def test_archive_flag_and_exit_codes(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d) / "root"
            root.mkdir()
            clean, bad = Path(d) / "clean.tar.gz", Path(d) / "bad.tar.gz"
            make_tar(clean, {"irrigator/README.md": TEXT})
            make_tar(bad, {"irrigator/bin/irrigator": ELF})
            rc, out = run_main(["--root", str(root), "--archive", str(clean), "--allow-missing", "--json"])
            self.assertEqual(rc, 0, out)
            rc, out = run_main(["--root", str(root), "--archive", str(bad), "--allow-missing", "--json"])
            self.assertEqual(rc, 1)
            self.assertTrue(any(r["id"] == "tree.binary" and r["level"] == "FAIL" for r in json.loads(out)["results"]))

    def test_explicit_root_with_binary_exits_nonzero(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "bin/irrigator", ELF)
            rc, _out = run_main(["--root", d, "--allow-missing", "--json"])
            self.assertEqual(rc, 1)


CONSUMER_PATH = HERE / 'plugin-consumer-test.py'
NL = chr(10)


def load_consumer():
    spec = importlib.util.spec_from_file_location('plugin_consumer_test', str(CONSUMER_PATH))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class SwarmSkillSet(unittest.TestCase):
    def test_swarm_is_an_expected_skill(self):
        self.assertIn('swarm', V.SKILLS)
        self.assertEqual(len(V.SKILLS), len(set(V.SKILLS)))
        self.assertIn('skills/swarm/SKILL.md', V.FROZEN_FILES)

    def test_every_skill_has_concepts(self):
        self.assertEqual(set(V.CONCEPTS), set(V.SKILLS))

    def test_missing_swarm_skill_fails_unless_partial(self):
        with tempfile.TemporaryDirectory() as d:
            c, rep = make_ctx(d, allow_missing=False)
            V.t_files(c)
            missing = [r for r in rows(rep, 'FAIL', 'files.exist') if 'skills/swarm/SKILL.md' in r['message']]
            self.assertTrue(missing)
            c, rep = make_ctx(d, allow_missing=True)
            V.t_files(c)
            self.assertEqual(rows(rep, 'FAIL', 'files.exist'), [])

    def test_swarm_frontmatter_is_checked(self):
        good = NL.join(['---', 'name: swarm', 'description: Entry point for swarm shaped work with Irrigator', '---',
                        'Use irrigator only after budget authorization. Independent acceptance applies.']) + NL
        for text, want in ((good, True), (good.replace('name: swarm', 'name: other'), False)):
            with tempfile.TemporaryDirectory() as d:
                put(d, 'skills/swarm/SKILL.md', text)
                c, rep = make_ctx(d)
                V.t_skills(c)
                self.assertEqual(not rows(rep, 'FAIL', 'skill.swarm'), want, text)

    def test_scope_note_denies_native_discovery(self):
        self.assertIn('not exercised', V.SCOPE_NOTE)


@unittest.skipUnless(CONSUMER_PATH.is_file(), 'plugin-consumer-test.py is not in this tree')
class InstalledConsumer(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.C = load_consumer()

    def quiet_main(self, argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = self.C.main(argv)
        return rc, out.getvalue()

    def test_skill_lists_agree(self):
        self.assertEqual(tuple(self.C.SKILLS), tuple(V.SKILLS))

    def test_member_names(self):
        for bad in ('../x', '/abs/x', 'a//b', 'a/./b', ''):
            self.assertTrue(self.C.member_problem(bad), bad)
        self.assertIsNone(self.C.member_problem('irrigator-plugin-1.0.0/README.md'))

    def test_extract_clean_and_hostile_archives(self):
        with tempfile.TemporaryDirectory() as d:
            good = Path(d) / 'good.tar.gz'
            make_tar(good, {'irrigator-plugin-1.0.0/README.md': TEXT})
            dest = Path(d) / 'out'
            dest.mkdir()
            top = self.C.extract_archive(str(good), dest)
            self.assertTrue((top / 'README.md').is_file())
            hostile = (('traversal', {'../evil.txt': TEXT}), ('two-tops', {'a/x.md': TEXT, 'b/y.md': TEXT}),
                       ('flat', {'x.md': TEXT}), ('absolute', {'/abs/x.md': TEXT}))
            for name, members in hostile:
                bad = Path(d) / (name + '.zip')
                make_zip(bad, members)
                dest2 = Path(d) / ('out-' + name)
                dest2.mkdir()
                with self.assertRaises(ValueError):
                    self.C.extract_archive(str(bad), dest2)
            self.assertFalse((Path(d) / 'evil.txt').exists())

    def test_digest_detects_writes_into_the_bundle(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, 'a.txt', 'one')
            before = self.C.digest_tree(Path(d))
            self.assertEqual(before, self.C.digest_tree(Path(d)))
            put(d, '__pycache__/x.pyc', b'x')
            self.assertNotEqual(before, self.C.digest_tree(Path(d)))

    def test_binary_must_live_outside_the_bundle(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d) / 'bundle'
            root.mkdir()
            inside = put(root, 'bin/irrigator', b'x')
            outside = put(d, 'irrigator', b'x')
            self.assertTrue(self.C.inside_root(root, inside))
            self.assertFalse(self.C.inside_root(root, outside))

    def test_inputs_are_explicit(self):
        with self.assertRaises(SystemExit) as cm, contextlib.redirect_stderr(io.StringIO()):
            self.C.main([])
        self.assertEqual(cm.exception.code, 2)
        with tempfile.TemporaryDirectory() as d:
            rc, _out = self.quiet_main(['--root', d, '--binary', 'relative/irrigator'])
            self.assertEqual(rc, 2)
            rc, _out = self.quiet_main(['--root', str(Path(d) / 'missing')])
            self.assertEqual(rc, 2)

    def test_incomplete_bundle_fails_and_states_scope(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, 'README.md', TEXT)
            rc, out = self.quiet_main(['--root', d, '--json'])
            self.assertEqual(rc, 1)
            rep = json.loads(out)
            self.assertEqual(rep['layer'], 'installed-consumer')
            self.assertIn('not exercised', rep['scope'])
            self.assertTrue(any(r['level'] == 'SKIP' and r['id'] == 'consumer.smoke' for r in rep['results']))
            self.assertTrue(any(r['level'] == 'FAIL' and r['id'] == 'bundle.layout' for r in rep['results']))



class CurrentPackagedCLIContract(unittest.TestCase):
    def test_current_commands_and_schedule_are_known(self):
        for command in ["irrigator retire run --reason test --session-registry reg",
                        "irrigator delivery-status run",
                        "irrigator apply plan --run run --schedule breadth",
                        "irrigator resume run --schedule depth"]:
            self.assertFalse(V.validate_invocation(V.parse_invocation(command)), command)

    def test_unimplemented_command_is_not_whitelisted(self):
        self.assertTrue(V.validate_invocation(V.parse_invocation("irrigator revalidate run node")))

    def test_explicit_proposal_does_not_exempt_later_real_command(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "README.md", "Proposed bounded command, to be implemented separately and\nreviewed:\n\n`irrigator revalidate run node`\n\nRun this now: `irrigator revalidate run node`\n")
            ctx, rep = make_ctx(d)
            V.t_commands(ctx)
            self.assertEqual(len(rows(rep, "SKIP", "cli.proposed-example")), 1)
            self.assertEqual(len(rows(rep, "FAIL", "cli.invocation")), 1)

    def test_vague_proposed_word_does_not_skip_unknown_command(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "README.md", "Proposed workflow:\n\n`irrigator nonexistent run`\n")
            ctx, rep = make_ctx(d)
            V.t_commands(ctx)
            self.assertEqual(len(rows(rep, "FAIL", "cli.invocation")), 1)

class CurrentContracts(unittest.TestCase):
    def test_current_commands_and_invalid_subcommands(self):
        for command in ("backup create --run run --out backup", "recover --run run --out recovered --migrate-to 5", "billing reconcile run receipt --attest-external-billing", "campaign init campaign --budget 1", "mailbox process run", "profiles resolve --catalog catalog --profile mock", "context create --workspace workspace --out context", "helpers run node --operation op --attempt 1 --request req", "turn-shape 2x3x1 --json", "compose workflow --out plan", "update-check --manifest m --signature s --artifact a --public-key k --installed-sequence 1"):
            self.assertFalse(V.validate_invocation(V.parse_invocation("irrigator " + command)), command)
        for command in ("backup destroy run", "billing refund run", "profiles resolve --password secret", "context create --grant-permissions"):
            self.assertTrue(V.validate_invocation(V.parse_invocation("irrigator " + command)), command)

    def test_generated_metadata_retires_panel_flags_and_keeps_service_flags(self):
        service = "irrigator serve --dir service --model model --claude /trusted/stub --listen 127.0.0.1:0"
        self.assertFalse(V.validate_invocation(V.parse_invocation(service)), service)
        for command in ("irrigator panel --run run --listen 127.0.0.1:0",
                        "irrigator panel --follow-session registry",
                        "irrigator panel --imaginary-flag"):
            self.assertTrue(V.validate_invocation(V.parse_invocation(command)), command)

    def test_metadata_rejects_unknown_schema_and_duplicates(self):
        good = {"schema": "irrigator.cli/v1", "commands": [{"name": "sample", "flags": [{"name": "value"}]}]}
        flags, subs, _ = V.parse_cli_contract(good)
        self.assertFalse(V.validate_invocation(["irrigator", "sample", "--value", "x"], flags, subs))
        self.assertTrue(V.validate_invocation(["irrigator", "apply"], flags, subs))
        for value in ({"schema": "future", "commands": []}, {"schema": "irrigator.cli/v1", "commands": good["commands"] * 2}, {"schema": "irrigator.cli/v1", "commands": [{"name": "sample", "flags": [{"name": "value"}, {"name": "value"}]}]}):
            with self.assertRaises(ValueError):
                V.parse_cli_contract(value)

    def test_call_budget_alias_and_conflict(self):
        for command in ("apply plan --run run --backend mock --call-budget 0.25", "deliver plan --run run --backend mock --call-budget 0.25", "resume run --call-budget 0.25"):
            self.assertFalse(V.validate_invocation(V.parse_invocation("irrigator " + command)), command)
        self.assertTrue(V.validate_invocation(V.parse_invocation("irrigator resume run --call-budget 0.25 --budget 0.25")))
        with tempfile.TemporaryDirectory() as d:
            put(d, "skills/run/SKILL.md", "```sh\nirrigator apply plan.json --run run --call-budget 0.25\n```\n")
            c, rep = make_ctx(d); V.t_commands(c)
            self.assertTrue(rows(rep, "WARN", "cli.backend-selection"))

    def test_versioned_plans_and_fragments(self):
        with tempfile.TemporaryDirectory() as d:
            for version in [None] + ["swarm/v%d" % n for n in range(1, 7)] + ["swarm/v99"]:
                plan = {"tasks": []}
                if version is not None:
                    plan["api_version"] = version
                put(d, "example.md", "```json\n" + json.dumps(plan) + "\n```\n")
                c, rep = make_ctx(d)
                V.t_json_examples(c)
                self.assertEqual(bool(rows(rep, "FAIL", "display.example-api")), version == "swarm/v99")

    def test_mit_claim_requires_notice_and_other_license_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "README.md", "MIT License\n")
            c, rep = make_ctx(d); V.t_claims(c)
            self.assertTrue(rows(rep, "FAIL", "claims.license"))
            put(d, "LICENSE", "MIT License\nPermission is hereby granted, free of charge\nTHE SOFTWARE IS PROVIDED")
            c, rep = make_ctx(d); V.t_claims(c)
            self.assertFalse(rows(rep, "FAIL", "claims.license"))
            put(d, "README.md", "licensed under GPL\n")
            c, rep = make_ctx(d); V.t_claims(c)
            self.assertTrue(rows(rep, "FAIL", "claims.license"))

    def test_source_build_is_pruned_but_package_build_is_scanned(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "build/deps/evil.md", "`irrigator unknown-command`\n")
            put(d, "build/bin", bytes.fromhex("7f454c46"))
            c, rep = make_ctx(d); V.t_commands(c); V.scan_root(rep, Path(d))
            self.assertTrue(rows(rep, "FAIL", "tree.binary"))
            self.assertTrue(rows(rep, "FAIL", "cli.invocation"))
            put(d, "main.go", "package main"); put(d, "go.mod", "module example"); put(d, ".git/HEAD", "ref: refs/heads/main")
            c, rep = make_ctx(d); V.t_commands(c); V.scan_root(rep, Path(d))
            self.assertFalse(rows(rep, "FAIL"))

    def test_package_directory_symlink_is_reported_without_descent(self):
        with tempfile.TemporaryDirectory() as d, tempfile.TemporaryDirectory() as outside:
            put(outside, "bad", bytes.fromhex("7f454c46"))
            (Path(d) / "linked").symlink_to(outside, target_is_directory=True)
            rep = V.Reporter(); V.scan_root(rep, Path(d))
            self.assertTrue(rows(rep, "WARN", "tree.symlink"))
            self.assertFalse(rows(rep, "FAIL", "tree.binary"))

    def test_archive_only_checks_archived_docs(self):
        with tempfile.TemporaryDirectory() as d:
            archive = Path(d) / "bad.zip"
            make_zip(archive, {"plugin/README.md": b"`irrigator unknown-command`\n"})
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = V.main(["--archive", str(archive), "--allow-missing", "--json"])
            report = json.loads(out.getvalue())
            self.assertEqual(code, 1)
            self.assertTrue(any(x["id"] == "cli.invocation" for x in report["results"]))
            self.assertNotEqual(Path(report["root"]), ROOT)

    def test_wrapped_denial_does_not_mask_bypass(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "README.md", "Neither digest is a\nsignature or a permission grant.\n\nWe bypass host permission prompts.\n")
            c, rep = make_ctx(d); V.t_claims(c)
            failures = rows(rep, "FAIL", "claims.permission")
            self.assertTrue(failures)
            self.assertTrue(all(":4 " in x["message"] for x in failures))

    def test_explicit_wrong_shape_and_missing_backend_still_flagged(self):
        with tempfile.TemporaryDirectory() as d:
            put(d, "README.md", "25x5x2 = 125\n")
            put(d, "skills/run/SKILL.md", "```sh\nirrigator apply actual.json --run run\n```\n")
            c, rep = make_ctx(d); V.t_notation(c); V.t_commands(c)
            self.assertTrue(rows(rep, "FAIL", "shape.line"))
            self.assertTrue(rows(rep, "WARN", "cli.backend-selection"))

CONTRACT_WITH_MCP = '{"commands": [{"name": "mcp"}]}'


class McpAndDeniedListRules(unittest.TestCase):
    def claims(self, files):
        with tempfile.TemporaryDirectory() as d:
            for rel, data in files.items():
                put(d, rel, data)
            c, rep = make_ctx(d)
            V.t_claims(c)
            return rows(rep, "FAIL", "claims.")

    def test_mcp_claim_needs_backing(self):
        line = "See the [MCP server](mcp.md).\n"
        self.assertTrue(self.claims({"docs/a.md": line}))
        self.assertTrue(self.claims({"docs/a.md": line, "docs/mcp.md": "x", "verification/cli-contract.json": '{"commands": []}'}))
        backing = {"docs/mcp.md": "# MCP server\n", "verification/cli-contract.json": CONTRACT_WITH_MCP}
        self.assertEqual(self.claims(dict(backing, **{"docs/a.md": line})), [])
        # an unlinked claim stays rejected even when backed
        self.assertTrue(self.claims(dict(backing, **{"docs/a.md": "Ships an MCP server with admin tools.\n"})))

    def test_denied_list_is_exempt_but_publishing_claim_is_not(self):
        self.assertEqual(self.claims({"docs/a.md": "Denied: `git push`, `rm`.\n"}), [])
        self.assertTrue(self.claims({"docs/a.md": "Irrigator runs `git push` after the run.\n"}))

    def test_node_modules_is_skipped(self):
        self.assertEqual(self.claims({"examples/node_modules/p/README.md": "Run `git push` and `npm publish`.\n"}), [])

    def test_manifest_allows_only_read_only_mcp_entry(self):
        self.assertTrue(V.mcp_server_entry_ok({"command": "irrigator", "args": ["mcp"]}))
        self.assertFalse(V.mcp_server_entry_ok({"command": "irrigator", "args": ["mcp", "--allow-control"]}))
        self.assertFalse(V.mcp_server_entry_ok({"command": "sh", "args": ["mcp"]}))
        self.assertFalse(V.mcp_server_entry_ok({"command": "irrigator", "args": ["serve"]}))
        self.assertFalse(V.mcp_server_entry_ok({"command": "irrigator", "args": ["mcp"], "env": {"A": "b"}}))

    def manifest_fails(self, servers):
        with tempfile.TemporaryDirectory() as d:
            put(d, "docs/mcp.md", "x")
            put(d, "verification/cli-contract.json", CONTRACT_WITH_MCP)
            put(d, ".claude-plugin/plugin.json", json.dumps({"name": "irrigator", "version": "1.0.0", "description": "d" * 30, "mcpServers": servers}))
            c, rep = make_ctx(d)
            V.t_manifests(c)
            return rows(rep, "FAIL", "claims.manifest")

    def test_manifest_mcp_servers_validated(self):
        self.assertEqual(self.manifest_fails({"irrigator": {"command": "irrigator", "args": ["mcp"]}}), [])
        self.assertTrue(self.manifest_fails({"irrigator": {"command": "irrigator", "args": ["mcp", "--allow-control"]}}))
        self.assertTrue(self.manifest_fails({"irrigator": {"command": "irrigator", "args": ["mcp"]}, "other": {"command": "x"}}))


if __name__ == "__main__":
    unittest.main(verbosity=2 if "-v" in sys.argv else 1)
