#!/usr/bin/env python3
"""Render a stable Homebrew formula from four checked, frozen CLI archives.

Does not publish, install, overwrite a formula, or claim native execution of
cross-platform archives. Run the native candidate acceptance jobs first. Promote
the reviewed output to grainulation/homebrew, the sole maintained tap.
"""
import argparse
import hashlib
import importlib.util
from pathlib import Path
import re

SPEC = importlib.util.spec_from_file_location("archive_checker", Path(__file__).with_name("check_runtime_archive.py"))
checker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(checker)

TARGETS = ("darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64")
BEGIN = "  # BEGIN GENERATED STABLE RELEASE"
END = "  # END GENERATED STABLE RELEASE"
TEMPLATE = Path(__file__).with_name("templates") / "irrigator.rb.in"


def render(archives, version, commit, published_checksums=None):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("version must be a plain MAJOR.MINOR.PATCH release")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("commit must be the full frozen Git SHA")
    hashes = {}
    source_hash = None
    for archive in archives:
        archive = Path(archive)
        before = archive.read_bytes()
        _, provenance = checker.validate(archive)
        if archive.read_bytes() != before:
            raise ValueError("archive changed during validation")
        target = provenance.get("target")
        if target not in TARGETS or target in hashes:
            raise ValueError("unsupported or duplicate archive target")
        if provenance.get("version") != version or provenance.get("commit") != commit:
            raise ValueError("archive differs from frozen version or commit")
        expected_name = f"irrigator-{version}-{target.replace('/', '-')}.tar.gz"
        if archive.name != expected_name:
            raise ValueError("archive filename differs from release asset name")
        current_source = provenance.get("source_sha256", "")
        if not re.fullmatch(r"[0-9a-f]{64}", current_source):
            raise ValueError("archive lacks a source identity")
        if source_hash is not None and current_source != source_hash:
            raise ValueError("archives have different source identities")
        source_hash = current_source
        hashes[target] = hashlib.sha256(before).hexdigest()
    if set(hashes) != set(TARGETS):
        raise ValueError("all four native candidate archives are required")

    if published_checksums is not None:
        expected = {}
        for line in Path(published_checksums).read_text(encoding="ascii").splitlines():
            match = re.fullmatch(r"([0-9a-f]{64})  (irrigator-[^/\\\s]+\.tar\.gz)", line)
            if match is None or match[2] in expected:
                raise ValueError("invalid or duplicate published checksum")
            expected[match[2]] = match[1]
        actual = {f"irrigator-{version}-{target.replace('/', '-')}.tar.gz": digest
                  for target, digest in hashes.items()}
        if expected != actual:
            raise ValueError("published checksums differ from the four release archives")

    lines = [f"  # Frozen runtime commit: {commit}",
             f"  # Source SHA-256: {source_hash}", f'  version "{version}"', ""]
    for goos, platform in (("darwin", "macos"), ("linux", "linux")):
        lines.append(f"  on_{platform} do")
        for goarch, arch in (("arm64", "arm"), ("amd64", "intel")):
            target = f"{goos}/{goarch}"
            name = f"irrigator-{version}-{goos}-{goarch}.tar.gz"
            lines.extend([
                f"    on_{arch} do",
                f'      url "https://github.com/grainulation/irrigator/releases/download/v{version}/{name}"',
                f'      sha256 "{hashes[target]}"',
                "    end",
            ])
        lines.extend(["  end", ""])
    template = TEMPLATE.read_text(encoding="utf-8")
    if template.count(BEGIN) != 1 or template.count(END) != 1:
        raise ValueError("expected one generated stable release section")
    start, stop = template.index(BEGIN), template.index(END) + len(END)
    if stop <= start:
        raise ValueError("invalid stable release section order")
    return template[:start] + BEGIN + "\n" + "\n".join(lines).rstrip() + "\n" + END + template[stop:]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, action="append", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--published-checksums", type=Path,
                        help="downloaded release SHA256SUMS; require exact archive matches before promotion")
    parser.add_argument("--out", type=Path, required=True, help="new formula file; existing files are refused")
    args = parser.parse_args()
    try:
        formula = render(args.archive, args.version, args.commit, args.published_checksums)
        with args.out.open("x", encoding="utf-8") as output:
            output.write(formula)
    except (ValueError, KeyError, OSError) as error:
        parser.exit(1, f"Homebrew formula generation failed: {error}\n")
    print(f"Homebrew formula prepared: {args.out}; review before promoting; nothing published")


if __name__ == "__main__":
    main()
