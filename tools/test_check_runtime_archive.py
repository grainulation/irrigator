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


if __name__ == "__main__":
    unittest.main()
