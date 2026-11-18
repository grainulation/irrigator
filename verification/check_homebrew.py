#!/usr/bin/env python3
"""Exercise a Homebrew formula recipe in a private prefix and owned temporary tap."""
import argparse
import importlib.util
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def check(formula, archive=None):
    brew = shutil.which("brew")
    if not brew:
        raise ValueError("Homebrew is required for check-homebrew")
    go = shutil.which("go") if archive is None else None
    if archive is None and not go:
        raise ValueError("a Go compiler is required to check the HEAD recipe")
    with tempfile.TemporaryDirectory(prefix="irrigator-brew-check-") as directory:
        private = Path(directory).resolve()
        env = {**os.environ, "HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_NO_ANALYTICS": "1",
               "HOMEBREW_DEVELOPER": "1", "XDG_CONFIG_HOME": str(private / "config"),
               "HOMEBREW_LOGS": str(private / "logs")}
        source = ROOT
        spec = "head"
        if archive is not None:
            module_spec = importlib.util.spec_from_file_location("checker", ROOT / "tools/check_runtime_archive.py")
            checker = importlib.util.module_from_spec(module_spec)
            module_spec.loader.exec_module(checker)
            files, provenance = checker.validate(archive)
            goos = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
            arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
            if provenance["target"] != f"{goos}/{arch}":
                raise ValueError("Homebrew candidate must match the native platform")
            source = private / "candidate"
            source.mkdir()
            for name, data in files.items():
                (source / name).write_bytes(data)
            (source / "irrigator").chmod(0o755)
            spec = "stable"
        tap = f"grainulation/irrigator-check-{os.getpid()}"
        run = lambda args, **kwargs: subprocess.run([brew, *args], env=env, check=True, **kwargs)
        run(["tap-new", "--no-git", tap])
        try:
            tap_root = Path(run(["--repository", tap], stdout=subprocess.PIPE, text=True).stdout.strip())
            installed_formula = tap_root / "Formula/irrigator.rb"
            shutil.copyfile(formula, installed_formula)
            # Homebrew 7 requires explicit formula trust. Store only this owned
            # test formula's trust in the private config; older Homebrew has no
            # trust command. Never disable its trust or path restrictions.
            help_result = subprocess.run([brew, "help", "trust"], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if help_result.returncode == 0:
                run(["trust", "--formula", f"{tap}/irrigator"])
            args = ["ruby", str(ROOT / "verification/homebrew-formula-test.rb"),
                    str(installed_formula), str(source), spec,
                    str(archive.resolve()) if archive is not None else "", go or ""]
            run(args)
        finally:
            run(["untap", tap])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--formula", type=Path, default=ROOT / "Formula/irrigator.rb")
    parser.add_argument("--archive", type=Path, help="checked native candidate for the generated stable recipe")
    args = parser.parse_args()
    try:
        check(args.formula, args.archive)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"Homebrew recipe check failed: {error}\n")


if __name__ == "__main__":
    main()
