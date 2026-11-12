#!/usr/bin/env python3
"""Build deterministic, offline CLI archives from an exact Git source snapshot."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
import tempfile

TARGETS = ("darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64")
ROOT_FILES = {"go.mod", "go.sum", "main.go", "README.md", "LICENSE", "plugin.json"}
SOURCE_DIRS = {"internal", "examples"}


def run(args, **kwargs):
    return subprocess.check_output(args, stderr=subprocess.PIPE, **kwargs)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def source_snapshot(root, revision=None):
    if revision is None and run(["git", "status", "--porcelain", "--untracked-files=all"], cwd=root).strip():
        raise ValueError("working tree is not clean; commit changes or explicitly select --revision")
    commit = run(["git", "rev-parse", "--verify", (revision or "HEAD") + "^{commit}"], cwd=root).decode().strip()
    entries = run(["git", "ls-tree", "-rz", "--full-tree", commit], cwd=root).split(b"\0")
    files = {}
    for entry in entries:
        if not entry:
            continue
        meta, raw = entry.split(b"\t", 1)
        name = raw.decode("utf-8")
        path = PurePosixPath(name)
        if name not in ROOT_FILES and path.parts[0] not in SOURCE_DIRS:
            continue
        mode, kind, oid = meta.split()
        if mode not in (b"100644", b"100755") or kind != b"blob":
            raise ValueError("source snapshot contains a nonregular input")
        if path.is_absolute() or ".." in path.parts or "\\" in name:
            raise ValueError("source snapshot contains an unsafe path")
        files[name] = run(["git", "cat-file", "blob", oid.decode()], cwd=root)
    for required in ("go.mod", "README.md", "LICENSE", "plugin.json", "main.go"):
        if required not in files:
            raise ValueError("source snapshot lacks required release inputs")
    return commit, files


def archive_bytes(prefix, members):
    buffer = io.BytesIO()
    with gzip.GzipFile(fileobj=buffer, mode="wb", filename="", mtime=0) as zipped:
        with tarfile.open(fileobj=zipped, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name, data in sorted(members.items()):
                info = tarfile.TarInfo(prefix + "/" + name)
                info.size = len(data)
                info.mode = 0o755 if name == "irrigator" else 0o644
                info.mtime = 0
                archive.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


def json_bytes(value):
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


def build(root, out, go, revision=None, targets=TARGETS):
    # Refuse symlinked output ancestry and any preexisting destination.
    out = Path(os.path.abspath(out))
    if any(p.is_symlink() for p in (out, *out.parents)):
        raise ValueError("output path must not traverse symlinks")
    if out.exists():
        raise ValueError("output directory already exists")
    commit, files = source_snapshot(root, revision)
    version = json.loads(files["plugin.json"])["version"]
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", version):
        raise ValueError("invalid manifest release version")
    source_manifest = [{"path": k, "sha256": digest(v), "bytes": len(v)} for k, v in sorted(files.items())]
    source_hash = digest(json_bytes(source_manifest))
    go = str(Path(go).resolve())
    env = {k: os.environ[k] for k in ("PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE") if k in os.environ}
    env.update({"CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOFLAGS": "", "GOENV": "off"})
    toolchain = run([go, "version"], env=env).decode().strip()
    if not re.match(r"go version go[0-9.]+ (darwin|linux)/(amd64|arm64)$", toolchain):
        raise ValueError("unexpected Go version identity")
    with tempfile.TemporaryDirectory(prefix="irrigator-runtime-") as temp:
        workspace = Path(temp) / "source"
        workspace.mkdir()
        for name, data in files.items():
            target = workspace / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        artifacts = {}
        for target in targets:
            if target not in TARGETS:
                raise ValueError("unsupported target")
            goos, goarch = target.split("/")
            binary = Path(temp) / "irrigator"
            run([go, "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", str(binary), "."], cwd=workspace, env={**env, "GOOS": goos, "GOARCH": goarch})
            payload = binary.read_bytes()
            provenance = {"schema": "irrigator-runtime-release/v1", "version": version, "commit": commit, "source_sha256": source_hash, "source_files": source_manifest, "target": target, "toolchain": toolchain, "cgo": False, "binary_sha256": digest(payload), "snapshot": "committed Git bytes; working tree excluded", "published": False}
            members = {"INSTALL.txt": b"Irrigator CLI archive\n\nVerify: shasum -a 256 -c SHA256SUMS (Linux: sha256sum -c SHA256SUMS).\nInspect: ./irrigator help\nMock demo: ./irrigator demo --run ./first-demo\nInspect result: ./irrigator status ./first-demo\nInstall: mkdir -p \"$HOME/.local/bin\" && install -m 0755 irrigator \"$HOME/.local/bin/irrigator\"\nAdd $HOME/.local/bin to PATH. No Go installation is needed.\nUse a new run directory; mock demo makes no paid provider calls.\nThis archive is unsigned. Checksums detect changes, not publisher identity.\nREADME.md describes the source repository; relative documentation links\nrequire that checkout. Visit https://github.com/grainulation/irrigator.\n", "irrigator": payload, "LICENSE": files["LICENSE"], "README.md": files["README.md"], "provenance.json": json_bytes(provenance)}
            members["SHA256SUMS"] = "".join(f"{digest(v)}  {k}\n" for k, v in sorted(members.items())).encode()
            prefix = f"irrigator-{version}-{goos}-{goarch}"
            artifacts[prefix + ".tar.gz"] = archive_bytes(prefix, members)
        artifacts["SHA256SUMS"] = "".join(f"{digest(v)}  {k}\n" for k, v in sorted(artifacts.items())).encode()
        # All compilation completes before exclusive output creation. Never replace.
        out.mkdir(parents=False)
        for name, data in artifacts.items():
            with (out / name).open("xb") as stream:
                stream.write(data)
    return sorted(artifacts)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--out", type=Path, required=True, help="new directory under an existing parent")
    parser.add_argument("--go", required=True, help="Go executable; automatic toolchain download disabled")
    parser.add_argument("--revision", help="explicit committed snapshot; ignores working tree changes")
    parser.add_argument("--target", action="append", choices=TARGETS, help="default: all four targets")
    args = parser.parse_args()
    try:
        names = build(args.root, args.out, args.go, args.revision, args.target or TARGETS)
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        # Do not echo subprocess stderr: it can contain machine paths or source data.
        parser.exit(1, f"runtime packaging failed: {type(exc).__name__}; verify inputs and offline toolchain dependencies\n")
    print("\n".join(names))


if __name__ == "__main__":
    main()
