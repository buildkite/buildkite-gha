#!/usr/bin/env python3
"""Schedule unmodified imports against published releases, then check artifacts."""

import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

PLATFORMS = {"linux/amd64": "linux_x86_64", "linux/arm64": "linux_arm64",
             "darwin/arm64": "darwin_arm64", "windows/amd64": "windows_x86_64.exe"}


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
    if os.environ["PINNED_CONSUMER_MODE"] == "candidate-platforms":
        run("buildkite-agent", "pipeline", "upload", "--no-interpolation", input=json.dumps({"steps": [
            {"key": "runtime-candidate-build", "label": "Build candidate runtime assets",
             "plugins": [{"mise#a5845c5082d3a4fe36dd77ae74973dfc86fc91a2": {"version": "2026.10.17"}}],
             "command": "mise exec -- python3 .buildkite/pinned-runtime-consumer-proof.py publish-candidate",
             "timeout_in_minutes": 20},
            {"key": "runtime-consumer-importer", "label": "Import unchanged mixed-platform jobs",
             "depends_on": "runtime-candidate-build",
             "command": "python3 .buildkite/pinned-runtime-consumer-proof.py import-candidate",
             "timeout_in_minutes": 20, "retry": {"automatic": False}}]}))
        return
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


def publish_candidate():
    directory = Path(".pinned-runtime-candidate")
    directory.mkdir()
    release = {"tag_name": "v0.0.0-proof", "assets": []}
    for platform, suffix in PLATFORMS.items():
        goos, goarch = platform.split("/")
        path = directory / ("buildkite-gha_" + suffix)
        run("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-X main.version=0.0.0-proof",
            "-o", str(path), "./cmd/buildkite-gha", env=dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0"))
        run("buildkite-agent", "artifact", "upload", str(path))
        url = run("buildkite-agent", "artifact", "search", str(path), "--step", os.environ["BUILDKITE_JOB_ID"],
                  "--format", "%u", stdout=subprocess.PIPE).stdout.strip()
        assert url.startswith("https://") and "\n" not in url
        release["assets"].append({"name": path.name, "browser_download_url": url,
                                  "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()})
    metadata = directory / "release.json"
    metadata.write_text(json.dumps(release))
    run("buildkite-agent", "artifact", "upload", str(metadata))


def import_candidate():
    ref = os.environ["PINNED_CONSUMER_PLUGIN_REF"]
    assert re.fullmatch(r"[0-9a-f]{40}|v[0-9]+\.[0-9]+\.[0-9]+", ref)
    pinned = os.environ["PINNED_CONSUMER_EXPECT_PINNED"] == "1"
    source = Path(__file__).resolve()
    real_agent = shutil.which("buildkite-agent")
    with tempfile.TemporaryDirectory(prefix="candidate-platform-import-") as tmp:
        root = Path(tmp)
        run(real_agent, "artifact", "download", ".pinned-runtime-candidate/release.json", tmp,
            "--step", "runtime-candidate-build")
        release = json.loads((root / ".pinned-runtime-candidate/release.json").read_text())
        cached = root / "cache/releases/tags/v0.0.0-proof.json"
        cached.parent.mkdir(parents=True)
        cached.write_text(json.dumps(release))
        wrapper = root / "bin/buildkite-agent"
        wrapper.parent.mkdir()
        wrapper.write_text(f'#!/bin/sh\nexec python3 "{source}" capture "$@"\n')
        wrapper.chmod(0o755)
        config = {"version": "v0.0.0-proof", "workflow": ".github/workflows/pinned-runtime-platforms.yml",
                  "runners": [{"runs-on": label, "queue": os.environ["PINNED_PROOF_" + key + "_QUEUE"]}
                              for label, key in [("ubuntu-latest", "LINUX"), ("ubuntu-24.04-arm", "ARM"),
                                                 ("macos-14", "MAC"), ("windows-2022", "WINDOWS")]]}
        plugin = "github.com/buildkite-plugins/github-actions-buildkite-plugin#" + ref
        pipeline = root / "pipeline.yml"
        env = dict(os.environ, PATH=f"{wrapper.parent}:{os.environ['PATH']}", PROOF_REAL_AGENT=real_agent,
                   PROOF_PIPELINE_FILE=str(pipeline), BUILDKITE_GHA_CACHE_DIR=str(root / "cache"),
                   BUILDKITE_PLUGINS=json.dumps([{plugin: config}]), BUILDKITE_PLUGIN_VALIDATION="true",
                   BUILDKITE_CLEAN_CHECKOUT="false", BUILDKITE_GHA_TELEMETRY_DISABLED="true")
        run(real_agent, "bootstrap", "--skip-checkout", "--plugins-path", str(root / "plugins"),
            "--sockets-path", str(root / "sockets"), "--bin-path", str(wrapper.parent),
            "--plugin-validation", "--phases", "plugin,command", env=env)
        parsed = json.loads(run(real_agent, "pipeline", "upload", "--dry-run", "--no-interpolation",
                                "--format", "json", str(pipeline), stdout=subprocess.PIPE).stdout)
        steps = [step for group in parsed["steps"] for step in group.get("steps", [group])]
        assert len(steps) == 8
        assets = {a["name"]: a for a in release["assets"]}
        for step in steps:
            assert step.get("env", {}).get("BUILDKITE_GHA_PLAN_DIGEST"), "expected executable job"
            if pinned:
                assert not step.get("command") and len(step["plugins"]) == 1
                invocation = step["plugins"][0][plugin]["run"]
                asset = assets["buildkite-gha_" + PLATFORMS[invocation["runtime"]["platform"]]]
                assert "sha256:" + invocation["runtime"]["sha256"] == asset["digest"]
                assert invocation["runtime"]["url"] == asset["browser_download_url"]
                assert invocation["plan"]["producer"] == os.environ["BUILDKITE_JOB_ID"]
            else:
                assert step.get("command") and not step.get("plugins"), "legacy plugin received run mode"
        paths = run(real_agent, "artifact", "search", "*", "--step", os.environ["BUILDKITE_JOB_ID"],
                    "--format", "%p\\n", stdout=subprocess.PIPE).stdout.splitlines()
        assert len([p for p in paths if "/distributions/" in p]) == (0 if pinned else 4), paths
        receipt = Path("candidate-platform-inventory.json")
        receipt.write_text(json.dumps({"plugin": plugin, "pinned": pinned, "artifacts": sorted(paths)}, indent=2))
        run(real_agent, "artifact", "upload", str(receipt))
        # Submit the original bytes. No command, target, environment, or plugin changes.
        run(real_agent, "pipeline", "upload", "--no-interpolation", str(pipeline))
        print(f"Scheduled eight unchanged emitted jobs; pinned={pinned}; plugin={ref}")
        print("LIMIT: candidate binaries use fixture release metadata; runtime downloads and scheduled jobs are real")


def capture():
    if sys.argv[2:4] == ["pipeline", "upload"]:
        Path(os.environ["PROOF_PIPELINE_FILE"]).write_text(sys.stdin.read())
    else:
        os.execv(os.environ["PROOF_REAL_AGENT"], [os.environ["PROOF_REAL_AGENT"], *sys.argv[2:]])


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
    {"load": load, "local": local, "inventory": inventory, "plugin-platforms": plugin_platforms,
     "publish-candidate": publish_candidate, "import-candidate": import_candidate, "capture": capture}[sys.argv[1]]()
