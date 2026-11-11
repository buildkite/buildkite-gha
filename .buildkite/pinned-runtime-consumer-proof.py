#!/usr/bin/env python3
"""Schedule unmodified imports against published releases, then check artifacts."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def configuration():
    mode = os.environ["PINNED_CONSUMER_MODE"]
    assert mode in ("release", "old-plugin", "historical", "custom", "local", "mixed")
    config = {"workflow": ".github/workflows/pinned-runtime-consumer.yml",
              "runners": [{"runs-on": "ubuntu-latest", "queue": "hosted"}]}
    if mode not in ("old-plugin", "custom"):
        config["version"] = os.environ["PINNED_CONSUMER_VERSION"]
    if mode == "custom":
        config["binary"] = os.environ["PINNED_CONSUMER_BINARY_URL"]
    if mode == "mixed":
        config["workflow"] = ".buildkite/fixtures/pinned-runtime-mixed.yml"
        config["runners"].append({"runs-on": "macos-14", "queue": "mac-small"})
    return mode, config


def load():
    assert os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch", "Set GITHUB_EVENT_NAME=workflow_dispatch on the proof build"
    if os.environ["PINNED_CONSUMER_MODE"] == "plugin-platforms":
        run("buildkite-agent", "pipeline", "upload", "--no-interpolation", input=json.dumps({"steps": [{
            "key": "runtime-consumer-importer", "label": "Native pinned plugin consumers",
            "command": "python3 .buildkite/pinned-runtime-consumer-proof.py plugin-platforms",
            "timeout_in_minutes": 20, "retry": {"automatic": False}}]}))
        return
    mode, config = configuration()
    importer = {"key": "runtime-consumer-importer", "label": f"Runtime consumer: {mode}",
                "timeout_in_minutes": 15, "retry": {"automatic": False}}
    if mode == "local":
        importer["command"] = "python3 .buildkite/pinned-runtime-consumer-proof.py local"
    else:
        plugin = "github-actions#v0.14.0" if mode == "old-plugin" else "github-actions#v0.15.1"
        importer["plugins"] = [{plugin: config}]
        # The old plugin's prose requirements predate agent validation support.
        importer["env"] = {"BUILDKITE_PLUGIN_VALIDATION": "false" if mode == "old-plugin" else "true"}
    verify = {"label": "Check importer runtime artifact inventory", "depends_on": "runtime-consumer-importer",
              "command": "python3 .buildkite/pinned-runtime-consumer-proof.py inventory"}
    run("buildkite-agent", "pipeline", "upload", "--no-interpolation", input=json.dumps({"steps": [importer, verify]}))


def plugin_platforms():
    # Use this pipeline's native platform queues before the CLI emits the new
    # contract. The plugin proof preserves plans but adapts their delivery.
    ref = os.environ["PINNED_PROOF_PLUGIN_REF"]
    assert re.fullmatch(r"[0-9a-f]{40}|v[0-9]+\.[0-9]+\.[0-9]+", ref)
    with tempfile.TemporaryDirectory(prefix="pinned-plugin-proof-") as tmp:
        run("git", "clone", "--filter=blob:none", "--no-checkout",
            "https://github.com/buildkite-plugins/github-actions-buildkite-plugin.git", tmp)
        run("git", "fetch", "--depth=1", "origin", ref, cwd=tmp)
        run("git", "checkout", "--detach", "FETCH_HEAD", cwd=tmp)
        env = dict(os.environ, PINNED_PROOF_REPOSITORY="buildkite/buildkite-gha")
        run("python3", str(Path(tmp) / ".buildkite/pinned-runtime-proof.py"), "platforms", env=env)


def local():
    _, config = configuration()
    with tempfile.TemporaryDirectory(prefix="released-local-runtime-") as tmp:
        binary = Path(tmp) / "buildkite-gha"
        run("curl", "--fail", "--silent", "--show-error", "--location", "--proto", "=https",
            "--proto-redir", "=https", os.environ["PINNED_CONSUMER_BINARY_URL"], "--output", str(binary))
        assert hashlib.sha256(binary.read_bytes()).hexdigest() == os.environ["PINNED_CONSUMER_SHA256"]
        binary.chmod(0o755)
        env = dict(os.environ, BUILDKITE_PLUGIN_CONFIGURATION=json.dumps(config))
        env.pop("BUILDKITE_GHA_PLUGIN_RELEASE", None)
        run(str(binary), "--version")
        run(str(binary), "plugin", env=env)


def inventory():
    result = run("buildkite-agent", "artifact", "search", "*", "--step", "runtime-consumer-importer",
                 "--format", "%p\\n", stdout=subprocess.PIPE)
    paths = sorted(result.stdout.splitlines())
    distributions = [path for path in paths if "/distributions/" in path]
    assert len(distributions) == int(os.environ["PINNED_CONSUMER_DISTRIBUTIONS"]), paths
    assert any("/plans/" in path for path in paths), paths
    Path("consumer-inventory.json").write_text(json.dumps(paths, indent=2))
    run("buildkite-agent", "artifact", "upload", "consumer-inventory.json")
    print(f"PASS: {os.environ['PINNED_CONSUMER_MODE']} runtime distributions={len(distributions)}")


if __name__ == "__main__":
    {"load": load, "local": local, "inventory": inventory, "plugin-platforms": plugin_platforms}[sys.argv[1]]()
