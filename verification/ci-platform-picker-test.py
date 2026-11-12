"""Exercise CI platform selection against real, isolated Git changes."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / ".github/scripts/pick-platforms.sh"


class PlatformPicker(unittest.TestCase):
    def test_only_documentation_can_skip_macos(self):
        with tempfile.TemporaryDirectory(prefix="irrigator-platform-picker-") as name:
            scratch = Path(name)
            repo = scratch / "repo"
            repo.mkdir()
            environment = dict(os.environ, GIT_CONFIG_NOSYSTEM="1",
                               GIT_CONFIG_GLOBAL=os.devnull)

            def git(*args):
                return subprocess.check_output(
                    ["git", "-C", str(repo), *args],
                    env=environment, text=True, stderr=subprocess.DEVNULL).strip()

            def commit():
                git("add", "-A")
                git("-c", "user.name=CI fixture", "-c",
                    "user.email=ci@example.invalid", "commit", "-qm", "fixture")
                return git("rev-parse", "HEAD")

            def check(event, base, macos):
                output = scratch / "output"
                output.write_text("")
                env = dict(environment, EVENT=event, PR_BASE=base,
                           PUSH_BEFORE=base, GITHUB_OUTPUT=str(output))
                subprocess.run(["sh", str(SCRIPT)], cwd=repo, env=env,
                               check=True, capture_output=True, timeout=10)
                values = [line.removeprefix("os=") for line in
                          output.read_text().splitlines() if line.startswith("os=")]
                self.assertEqual(len(values), 1)
                expected = ["ubuntu-24.04", "macos-15"] if macos else ["ubuntu-24.04"]
                self.assertEqual(json.loads(values[0]), expected)

            git("init", "-q")
            (repo / "README.md").write_text("first\n")
            (repo / "source.go").write_text("package fixture\n")
            base = commit()
            (repo / "README.md").write_text("second\n")
            commit()
            check("pull_request", base, False)
            check("workflow_dispatch", base, True)
            check("pull_request", "unknown-base", True)
            check("push", "0" * 40, True)
            check("pull_request", git("rev-parse", "HEAD"), True)

            base = git("rev-parse", "HEAD")
            (repo / "source.go").write_text("package changed\n")
            commit()
            check("pull_request", base, True)

            # Rename detection must not hide the deletion of runtime code.
            base = git("rev-parse", "HEAD")
            (repo / "docs").mkdir()
            git("mv", "source.go", "docs/source.md")
            commit()
            check("pull_request", base, True)

            base = git("rev-parse", "HEAD")
            git("mv", "README.md", "docs/README.md")
            commit()
            check("pull_request", base, False)

            base = git("rev-parse", "HEAD")
            git("mv", "docs/source.md", "source.go")
            commit()
            check("pull_request", base, True)

            base = git("rev-parse", "HEAD")
            (repo / "source.go").unlink()
            commit()
            check("pull_request", base, True)


if __name__ == "__main__":
    unittest.main()
