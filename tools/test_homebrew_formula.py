#!/usr/bin/env python3
"""Pin release selection and reject corrupted/mixed candidates before publishing URLs."""
import json
from pathlib import Path
import sys
import subprocess
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "tools"))
import homebrew_formula as formula
import package_runtime as runtime

COMMIT = "a" * 40
SOURCE = "b" * 64


class HomebrewFormulaTest(unittest.TestCase):
    def archive(self, directory, target, *, commit=COMMIT, version="1.0.0", source=SOURCE, corrupt=False):
        members = {"irrigator": b"binary fixture", "LICENSE": b"MIT", "README.md": b"readme", "INSTALL.txt": b"install"}
        members["provenance.json"] = json.dumps({
            "schema": "irrigator-runtime-release/v1", "version": version,
            "commit": commit, "source_sha256": source, "target": target,
            "binary_sha256": runtime.digest(members["irrigator"]),
        }).encode()
        members["SHA256SUMS"] = "".join(f"{runtime.digest(value)}  {name}\n" for name, value in sorted(members.items())).encode()
        if corrupt:
            members["irrigator"] = b"corrupted payload"
        name = f"irrigator-{version}-{target.replace('/', '-')}.tar.gz"
        path = Path(directory) / name
        path.write_bytes(runtime.archive_bytes(name.removesuffix(".tar.gz"), members))
        return path

    def archives(self, directory, **last):
        return [self.archive(directory, target, **(last if index == 3 else {}))
                for index, target in enumerate(formula.TARGETS)]

    def test_pins_all_four_downloads_to_exact_checked_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            archives = self.archives(directory)
            rendered = formula.render(archives, "1.0.0", COMMIT)
            for archive in archives:
                self.assertIn(f"/v1.0.0/{archive.name}\"", rendered)
                self.assertIn(runtime.digest(archive.read_bytes()), rendered)
            self.assertEqual(rendered.count("sha256 "), 4)
            self.assertIn('head do', rendered)
            self.assertIn('depends_on "go" => :build', rendered)
            self.assertIn('  version "1.0.0"', rendered)
            self.assertEqual(rendered.count(formula.BEGIN), 1)
            self.assertIn(f"# Frozen runtime commit: {COMMIT}", rendered)
            self.assertIn(f"# Source SHA-256: {SOURCE}", rendered)

    def test_template_has_no_published_release_pins(self):
        template = formula.TEMPLATE.read_text()
        stable = template.split(formula.BEGIN, 1)[1].split(formula.END, 1)[0]
        self.assertNotIn("version ", stable)
        self.assertNotIn("sha256 ", stable)
        self.assertNotIn("url ", stable)
        self.assertEqual(formula.TEMPLATE.suffix, ".in")

    def test_recipe_checker_requires_explicit_formula_before_homebrew(self):
        result = subprocess.run(
            [sys.executable, "-I", str(ROOT / "verification/check_homebrew.py")],
            env={"PATH": ""}, text=True, capture_output=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("required: --formula", result.stderr)

    def test_published_promotion_requires_the_exact_complete_checksum_set(self):
        with tempfile.TemporaryDirectory() as directory:
            archives = self.archives(directory)
            sums = Path(directory) / "SHA256SUMS"
            lines = [f"{runtime.digest(archive.read_bytes())}  {archive.name}\n" for archive in archives]
            sums.write_text("".join(reversed(lines)))
            self.assertEqual(formula.render(archives, "1.0.0", COMMIT, sums),
                             formula.render(archives, "1.0.0", COMMIT))
            for invalid in (lines[:-1], lines + lines[:1],
                            ["c" * 64 + lines[0][64:], *lines[1:]],
                            [lines[0].replace("  irrigator", "  ../irrigator"), *lines[1:]],
                            lines + ["c" * 64 + "  unexpected.tar.gz\n"]):
                with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                    sums.write_text("".join(invalid))
                    formula.render(archives, "1.0.0", COMMIT, sums)

    def test_rejects_missing_duplicate_and_mixed_archives(self):
        with tempfile.TemporaryDirectory() as directory:
            archives = self.archives(directory)
            for selection in (archives[:-1], archives + archives[:1]):
                with self.assertRaises(ValueError):
                    formula.render(selection, "1.0.0", COMMIT)
            for override in ({"commit": "c" * 40}, {"version": "1.0.1"}, {"source": "d" * 64}, {"source": ""}, {"corrupt": True}):
                with self.subTest(override=override), self.assertRaises(ValueError):
                    formula.render(self.archives(directory, **override), "1.0.0", COMMIT)

    def test_updates_the_stable_stanza_without_duplicating_it(self):
        with tempfile.TemporaryDirectory() as directory:
            archives = self.archives(directory)
            rendered = formula.render(archives, "1.0.0", COMMIT)
            template = Path(directory) / "irrigator.rb"
            template.write_text(rendered)
            with mock.patch.object(formula, "TEMPLATE", template):
                self.assertEqual(formula.render(archives, "1.0.0", COMMIT), rendered)

    def test_rejects_invalid_release_identity_and_renamed_assets(self):
        with tempfile.TemporaryDirectory() as directory:
            archives = self.archives(directory)
            for version, commit in (("1.0.0-rc.1", COMMIT), ("1.0.0", "short"), ('1.0.0"; system "false', COMMIT)):
                with self.assertRaises(ValueError):
                    formula.render(archives, version, commit)
            renamed = archives[0].with_name("different-name.tar.gz")
            archives[0].rename(renamed)
            with self.assertRaises(ValueError):
                formula.render([renamed, *archives[1:]], "1.0.0", COMMIT)

    def test_refuses_overwriting_an_existing_formula(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory) / "existing.rb"
            out.write_text("preserve existing formula\n")
            argv = ["homebrew_formula.py", "--version", "1.0.0", "--commit", COMMIT, "--out", str(out)]
            for archive in self.archives(directory):
                argv += ["--archive", str(archive)]
            with mock.patch.object(sys, "argv", argv), self.assertRaises(SystemExit):
                formula.main()
            self.assertEqual(out.read_text(), "preserve existing formula\n")


if __name__ == "__main__":
    unittest.main()
