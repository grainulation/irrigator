#!/usr/bin/env python3
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("runtime_package", Path(__file__).with_name("package_runtime.py"))
pkg = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(pkg)


class RuntimePackageTest(unittest.TestCase):
    def repo(self, directory):
        root = Path(directory)
        subprocess.run(["git", "init", "-q", str(root)], check=True)
        for name, data in {"go.mod": "module test\n", "README.md": "readme", "LICENSE": "MIT", "plugin.json": json.dumps({"version": "1.0.0-rc.4"}), "main.go": "package main\n", "private.txt": "excluded"}.items():
            file = root / name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text(data)
        subprocess.run(["git", "add", "."], cwd=root, check=True)
        subprocess.run(["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], cwd=root, check=True)
        return root

    def test_snapshot_dirty_and_untracked(self):
        with tempfile.TemporaryDirectory() as d:
            root = self.repo(d)
            commit, files = pkg.source_snapshot(root)
            self.assertNotIn("private.txt", files)
            (root / "main.go").write_text("changed")
            with self.assertRaises(ValueError):
                pkg.source_snapshot(root)
            actual, snapshot = pkg.source_snapshot(root, commit)
            self.assertEqual(actual, commit)
            self.assertEqual(snapshot["main.go"], b"package main\n")
            subprocess.run(["git", "checkout", "--", "main.go"], cwd=root, check=True)
            (root / "untracked.go").write_text("untracked")
            with self.assertRaises(ValueError):
                pkg.source_snapshot(root)

    def test_source_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            root = self.repo(d)
            (root / "internal").mkdir()
            (root / "internal/secret").symlink_to("/outside")
            subprocess.run(["git", "add", "internal/secret"], cwd=root, check=True)
            subprocess.run(["git", "-c", "user.name=Fixture", "-c", "user.email=f@example.invalid", "commit", "-qm", "link"], cwd=root, check=True)
            with self.assertRaises(ValueError):
                pkg.source_snapshot(root)

    def test_archive_determinism_modes_and_members(self):
        members = {"irrigator": b"binary", "LICENSE": b"license"}
        first = pkg.archive_bytes("release", members)
        self.assertEqual(first, pkg.archive_bytes("release", dict(reversed(list(members.items())))))
        with tarfile.open(fileobj=io.BytesIO(first), mode="r:gz") as archive:
            self.assertEqual(archive.getnames(), ["release/LICENSE", "release/irrigator"])
            for member in archive:
                self.assertEqual(member.mtime, 0)
                self.assertEqual(member.uid, 0)
                self.assertEqual(member.uname, "")
                self.assertEqual(member.mode, 0o755 if member.name.endswith("irrigator") else 0o644)

    def test_build_provenance_checksums_and_environment(self):
        with tempfile.TemporaryDirectory() as d:
            d = str(Path(d).resolve())
            root = self.repo(Path(d) / "repo")
            real_run = pkg.run
            builds = []

            def fake_compiler(args, **kwargs):
                if len(args) > 1 and args[1] == "version":
                    return b"go version go1.27.2 darwin/arm64\n"
                if len(args) > 1 and args[1] == "build":
                    builds.append((args, kwargs["env"]))
                    Path(args[args.index("-o") + 1]).write_bytes(b"fake compiled binary")
                    return b""
                return real_run(args, **kwargs)

            with mock.patch.object(pkg, "run", side_effect=fake_compiler), mock.patch.dict(pkg.os.environ, {"SECRET_TOKEN": "must-not-pass", "GOFLAGS": "unsafe"}):
                for name in ("one", "two"):
                    pkg.build(root, Path(d) / name, "/fake/go", targets=("linux/arm64",))
            one, two = Path(d) / "one", Path(d) / "two"
            self.assertEqual({p.name: p.read_bytes() for p in one.iterdir()}, {p.name: p.read_bytes() for p in two.iterdir()})
            args, env = builds[0]
            self.assertNotIn("SECRET_TOKEN", env)
            self.assertEqual(env["GOFLAGS"], "")
            self.assertEqual(env["GOPROXY"], "off")
            self.assertEqual(env["CGO_ENABLED"], "0")
            archive = next(one.glob("*.tar.gz"))
            with tarfile.open(archive) as tar:
                entries = {m.name.split("/", 1)[1]: tar.extractfile(m).read() for m in tar}
            self.assertNotIn(d.encode(), entries["provenance.json"])
            provenance = json.loads(entries["provenance.json"])
            self.assertEqual(provenance["binary_sha256"], pkg.digest(entries["irrigator"]))
            for line in entries["SHA256SUMS"].decode().splitlines():
                expected, name = line.split("  ")
                self.assertEqual(expected, pkg.digest(entries[name]))

    def test_existing_output_refused_before_build(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.object(pkg, "source_snapshot") as snapshot:
            with self.assertRaises(ValueError):
                pkg.build(Path(d), Path(d), "go")
            snapshot.assert_not_called()

    def test_symlink_output_parent_refused(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "link").symlink_to(root, target_is_directory=True)
            with self.assertRaises(ValueError):
                pkg.build(root, root / "link/out", "go")


if __name__ == "__main__":
    unittest.main()
