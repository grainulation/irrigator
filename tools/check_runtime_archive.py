#!/usr/bin/env python3
"""Validate a CLI archive, then install and exercise it in a private temporary home."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import subprocess
import tarfile
import tempfile

MEMBERS = {"irrigator", "LICENSE", "README.md", "INSTALL.txt", "provenance.json", "SHA256SUMS"}
MAX_BYTES = 64 * 1024 * 1024


def validate(archive):
    files = {}
    prefix = None
    total = 0
    with tarfile.open(archive, "r:gz") as tar:
        for member in tar:
            path = PurePosixPath(member.name)
            if (not member.isfile() or len(path.parts) != 2 or path.is_absolute()
                    or str(path) != member.name or "\\" in member.name
                    or any(part in (".", "..") for part in path.parts)
                    or any(ord(char) < 32 for char in member.name)):
                raise ValueError("unsafe archive member")
            if prefix is None:
                prefix = path.parts[0]
            name = path.parts[1]
            if path.parts[0] != prefix or name not in MEMBERS or name in files:
                raise ValueError("unexpected or duplicate member")
            if member.mode != (0o755 if name == "irrigator" else 0o644):
                raise ValueError("unexpected member mode")
            total += member.size
            if member.size < 0 or total > MAX_BYTES:
                raise ValueError("archive exceeds size bound")
            files[name] = tar.extractfile(member).read(member.size + 1)
            if len(files[name]) != member.size:
                raise ValueError("member size mismatch")
    if set(files) != MEMBERS:
        raise ValueError("missing archive member")
    checked = set()
    for line in files["SHA256SUMS"].decode("ascii").splitlines():
        expected, name = line.split("  ")
        if name not in MEMBERS - {"SHA256SUMS"} or name in checked:
            raise ValueError("invalid checksum member")
        if hashlib.sha256(files[name]).hexdigest() != expected:
            raise ValueError("checksum mismatch")
        checked.add(name)
    if checked != MEMBERS - {"SHA256SUMS"}:
        raise ValueError("incomplete checksums")
    provenance = json.loads(files["provenance.json"])
    if provenance.get("schema") != "irrigator-runtime-release/v1":
        raise ValueError("invalid provenance schema")
    if hashlib.sha256(files["irrigator"]).hexdigest() != provenance.get("binary_sha256"):
        raise ValueError("binary identity mismatch")
    if prefix != "irrigator-" + provenance["version"] + "-" + provenance["target"].replace("/", "-"):
        raise ValueError("archive identity mismatch")
    return files, provenance


def accept(archive):
    files, provenance = validate(archive)
    native = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
    if provenance["target"] != f"{native}/{arch}":
        raise ValueError("archive does not match native platform")
    with tempfile.TemporaryDirectory(prefix="irrigator-archive-check-") as temporary:
        home = Path(temporary).resolve()
        extracted = home / "extracted"
        extracted.mkdir()
        for name, data in files.items():
            (extracted / name).write_bytes(data)
        prefix = home / ".local/bin"
        prefix.mkdir(parents=True)
        binary = prefix / "irrigator"
        binary.write_bytes(files["irrigator"])
        binary.chmod(0o755)
        env = {"HOME": str(home), "PATH": str(prefix) + os.pathsep + "/usr/bin:/bin", "IRRIGATOR_UI": "plain", "TMPDIR": str(home)}
        commands = [["version"], ["help"], ["demo", "--run", "mock-demo"], ["status", "mock-demo"], ["export", "mock-demo", "--out", "results.json"]]
        for command in commands:
            result = subprocess.run([str(binary), *command], cwd=home, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=90)
            if result.returncode:
                raise ValueError("packaged command failed: " + command[0])
        if not (home / "results.json").is_file():
            raise ValueError("export missing")
    return {"archive_sha256": hashlib.sha256(Path(archive).read_bytes()).hexdigest(), "commit": provenance["commit"], "target": provenance["target"], "commands": commands, "result": "PASS", "paid_calls": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    args = parser.parse_args()
    print(json.dumps(accept(args.archive), indent=2))


if __name__ == "__main__":
    main()
