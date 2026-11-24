import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


checker = module("check_runtime_archive")
packager = module("package_runtime")


class ArchiveCheckTest(unittest.TestCase):
    def members(self):
        members = {name: b"fixture" for name in checker.MEMBERS - {"SHA256SUMS"}}
        members["provenance.json"] = json.dumps({"schema": "irrigator-runtime-release/v1", "version": "1.0.0", "target": "linux/arm64", "binary_sha256": packager.digest(members["irrigator"])}).encode()
        members["SHA256SUMS"] = "".join(f"{packager.digest(data)}  {name}\n" for name, data in sorted(members.items())).encode()
        return members

    def check(self, data):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "archive.tar.gz"
            path.write_bytes(data)
            return checker.validate(path)

    def test_valid_checksums(self):
        members = self.members()
        files, identity = self.check(packager.archive_bytes("irrigator-1.0.0-linux-arm64", members))
        self.assertEqual(files, members)
        self.assertEqual(identity["target"], "linux/arm64")

    def test_tamper_and_missing_checksum(self):
        for change in ("tamper", "missing", "duplicate", "unexpected"):
            with self.subTest(change=change):
                members = self.members()
                if change == "tamper":
                    members["irrigator"] = b"changed"
                elif change == "missing":
                    members["SHA256SUMS"] = b""
                elif change == "duplicate":
                    members["SHA256SUMS"] *= 2
                else:
                    members["surprise"] = b"unexpected"
                with self.assertRaises(ValueError):
                    self.check(packager.archive_bytes("irrigator-1.0.0-linux-arm64", members))

    def test_unsafe_names_types_and_duplicate_members(self):
        for name, kind, count in [("../escape", tarfile.REGTYPE, 1), ("root/irrigator", tarfile.SYMTYPE, 1), ("root/irrigator", tarfile.FIFOTYPE, 1), ("root/irrigator", tarfile.REGTYPE, 2), ("/root/irrigator", tarfile.REGTYPE, 1)]:
            with self.subTest(name=name, kind=kind, count=count):
                buffer = io.BytesIO()
                with tarfile.open(fileobj=buffer, mode="w:gz") as tar:
                    for _ in range(count):
                        entry = tarfile.TarInfo(name)
                        entry.type = kind
                        entry.mode = 0o755
                        tar.addfile(entry, io.BytesIO())
                with self.assertRaises(ValueError):
                    self.check(buffer.getvalue())

    def executable_archive(self, directory, version_output):
        target = {"Darwin": "darwin", "Linux": "linux"}[checker.platform.system()] + "/" + {
            "arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64",
        }[checker.platform.machine()]
        members = self.members()
        # A real, free executable exercises accept() through extraction and
        # subprocess execution. The other commands are not runtime validation.
        members["irrigator"] = ("#!/bin/sh\ncase \"$1\" in\n"
                                "version) printf '%s' '" + version_output + "';;\n"
                                "export) printf '{}' > results.json;;\nesac\n").encode()
        members["provenance.json"] = json.dumps({
            "schema": "irrigator-runtime-release/v1", "version": "1.0.0",
            "target": target, "commit": "synthetic-fixture",
            "binary_sha256": packager.digest(members["irrigator"]),
        }).encode()
        members["SHA256SUMS"] = "".join(
            f"{packager.digest(data)}  {name}\n"
            for name, data in sorted(members.items()) if name != "SHA256SUMS"
        ).encode()
        path = Path(directory) / "archive.tar.gz"
        path.write_bytes(packager.archive_bytes("irrigator-1.0.0-" + target.replace("/", "-"), members))
        return path

    def test_accept_matching_executable_version(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = self.executable_archive(directory, "1.0.0\n")
            self.assertEqual(checker.accept(archive)["result"], "PASS")

    def test_accept_rejects_executable_version_drift(self):
        for output in ("1.0.0-rc.4\n", "1.0.0\nextra output\n", ""):
            with self.subTest(output=output), tempfile.TemporaryDirectory() as directory:
                archive = self.executable_archive(directory, output)
                with self.assertRaisesRegex(ValueError, "version does not match provenance"):
                    checker.accept(archive)


if __name__ == "__main__":
    unittest.main()
