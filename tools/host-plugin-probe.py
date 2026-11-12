#!/usr/bin/env python3
"""Model-free native plugin installation/discovery in an isolated Codex home.
Never alters the user's plugin configuration, copies credentials, or starts a turn.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import selectors
import shutil
import subprocess
import tempfile
import time


def run(argv, env, cwd):
    p = subprocess.run(argv, env=env, cwd=cwd, capture_output=True, text=True, timeout=45)
    result = {"argv": argv, "exit_code": p.returncode, "stdout": p.stdout, "stderr": p.stderr}
    if p.returncode:
        raise RuntimeError(json.dumps(result))
    return result


def rpc_probe(env, cwd, marketplace):
    with tempfile.TemporaryFile(mode="w+") as err:
        p = subprocess.Popen(["codex", "app-server", "--stdio"], env=env, cwd=cwd,
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=err, text=True)
        sel = selectors.DefaultSelector()
        sel.register(p.stdout, selectors.EVENT_READ)
        def call(i, method, params):
            p.stdin.write(json.dumps({"id": i, "method": method, "params": params}) + "\n")
            p.stdin.flush()
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                if not sel.select(max(0, deadline-time.monotonic())):
                    break
                line = p.stdout.readline()
                if not line:
                    break
                value = json.loads(line)
                if value.get("id") == i:
                    if "error" in value:
                        raise RuntimeError(json.dumps(value))
                    return value["result"]
            raise RuntimeError("RPC timeout: " + method)
        try:
            call(1, "initialize", {"clientInfo": {"name": "irrigator-host-probe", "version": "1"},
                                   "capabilities": {"experimentalApi": True}})
            p.stdin.write('{"method":"initialized","params":{}}\n')
            p.stdin.flush()
            plugin = call(2, "plugin/read", {"pluginName": "irrigator", "marketplacePath": str(marketplace)})
            skills = call(3, "skills/list", {"cwds": [str(cwd)], "forceReload": True})
            return {"plugin": plugin, "skills": skills}
        finally:
            sel.close()
            p.terminate()
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--out", type=Path, required=True, help="new evidence directory")
    args = parser.parse_args()
    source = args.root.resolve(strict=True)
    args.out.mkdir(parents=True, exist_ok=False)
    evidence = {"schema": "irrigator.host-probe/v1", "model_calls": 0,
                "skill_semantic_invocation": False, "user_configuration_modified": False,
                "sources": {str(p.relative_to(source)): hashlib.sha256(p.read_bytes()).hexdigest()
                            for p in source.glob("skills/*/SKILL.md")}}
    # These are the hosts' documented configuration roots, scoped only to subprocesses.
    with tempfile.TemporaryDirectory(prefix="irrigator-host-") as tmp:
        base = Path(tmp).resolve()
        stage = base / "plugin"
        stage.mkdir()
        for name in ("plugin.json", ".codex-plugin", ".claude-plugin", "skills"):
            item = source / name
            if item.is_dir():
                shutil.copytree(item, stage / name)
            else:
                shutil.copy2(item, stage / name)
        host_config = base / "codex-home"
        host_config.mkdir(mode=0o700)
        env = os.environ.copy()
        env["CODEX_HOME"] = str(host_config)
        evidence["codex_version"] = run(["codex", "--version"], env, base)["stdout"].strip()
        evidence["codex_install"] = run(["codex", "plugin", "marketplace", "add", str(stage), "--json"], env, base)
        evidence["codex_install_result"] = run(["codex", "plugin", "add", "irrigator@irrigator-local", "--json"], env, base)
        listing = run(["codex", "plugin", "list", "--json"], env, base)
        evidence["codex_plugins"] = json.loads(listing["stdout"])
        evidence["codex_native"] = rpc_probe(env, base, stage / ".claude-plugin/marketplace.json")
        summary = evidence["codex_native"]["plugin"]["plugin"]["summary"]
        if not summary["installed"] or not summary["enabled"]:
            raise RuntimeError("native host did not confirm installed and enabled")
        found = evidence["codex_native"]["plugin"]["plugin"]["skills"]
        expected = {"irrigator:" + p.parent.name for p in source.glob("skills/*/SKILL.md")}
        if {item["name"] for item in found} != expected:
            raise RuntimeError("native plugin skill inventory differs from source")
        rows = evidence["codex_native"]["skills"].get("data", [])
        if not rows or any(row.get("errors") for row in rows):
            raise RuntimeError("native skill loading reported errors or no results")
        loaded = {item["name"] for row in rows for item in row.get("skills", [])
                  if item.get("enabled") and item.get("pluginId") == "irrigator@irrigator-local"}
        if loaded != expected:
            raise RuntimeError("native enabled skills do not match the installed plugin")
        env["CLAUDE_CONFIG_DIR"] = str(base / "claude-config")
        evidence["claude_version"] = run(["claude", "--version"], env, base)["stdout"].strip()
        evidence["claude_session_inventory"] = run(["claude", "--plugin-dir", str(stage), "plugin", "details", "irrigator"], env, base)
        evidence["claude_manifest_validation"] = run(["claude", "plugin", "validate", str(stage)], env, base)
    evidence["isolated_install_removed_after_probe"] = True
    (args.out / "host-probe.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print("Native isolated install and session discovery recorded; no model/skill semantic invocation.")


if __name__ == "__main__":
    main()
