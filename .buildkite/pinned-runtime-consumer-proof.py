#!/usr/bin/env python3
"""Schedule unmodified imports against published releases, then check artifacts."""

import hashlib
import json
import os
from pathlib import Path
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
    {"load": load, "local": local, "inventory": inventory}[sys.argv[1]]()
