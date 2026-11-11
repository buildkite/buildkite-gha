#!/usr/bin/env python3
"""Opt-in real-transport proof; metadata and retry context remain test inputs."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path.cwd()
OUTPUT = ROOT / ".pinned-runtime-proof"
PLUGIN = "github.com/buildkite-plugins/github-actions-buildkite-plugin#v0.15.1"
MARKER = "PINNED_PROOF user=runner home=/home/runner value=42"


def capture(args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def metadata(key):
    return json.loads(capture(["buildkite-agent", "meta-data", "get", key]))


def save_metadata(key, value):
    subprocess.run(["buildkite-agent", "meta-data", "set", key, json.dumps(value)], check=True)


def upload(path):
    subprocess.run(["buildkite-agent", "artifact", "upload", str(path.relative_to(ROOT))], check=True)


def download(path, producer):
    subprocess.run(["buildkite-agent", "artifact", "download", path, ".", "--step", producer], check=True)
    return ROOT / path


def inventory(producer):
    paths = capture(["buildkite-agent", "artifact", "search", "*", "--step", producer, "--format", "%p\\n"])
    return sorted(paths.splitlines())


def publish():
    assets = {"producer": os.environ["BUILDKITE_JOB_ID"]}
    for name in ("baseline", "candidate"):
        path = OUTPUT / name
        upload(path)
        url = capture(["buildkite-agent", "artifact", "search", str(path.relative_to(ROOT)), "--step", assets["producer"], "--format", "%u"])
        assert url.startswith("https://") and "\n" not in url
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        assets[name] = {"url": url, "sha256": digest}
        print(f"{name} SHA256={digest}")
    save_metadata("pinned-runtime-proof-assets", assets)


def wrappers(directory):
    directory.parent.chmod(0o755)
    directory.mkdir()
    script = directory / "proof.py"
    shutil.copyfile(Path(__file__).resolve(), script)
    for name, mode in (("buildkite-agent", "agent"), ("curl", "curl")):
        wrapper = directory / name
        wrapper.write_text(f'#!/bin/sh\nexec python3 "{script}" {mode} "$@"\n')
        wrapper.chmod(0o755)


def import_workflow(name):
    assets = metadata("pinned-runtime-proof-assets")
    binary = download(f".pinned-runtime-proof/{name}", assets["producer"])
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == assets[name]["sha256"]
    binary.chmod(0o755)
    pipeline_path = OUTPUT / f"{name}-pipeline.yml"
    with tempfile.TemporaryDirectory(prefix="pinned-import-") as tmp:
        wrapper_dir = Path(tmp) / "bin"
        wrappers(wrapper_dir)
        env = dict(os.environ, PROOF_REAL_AGENT=shutil.which("buildkite-agent"),
                   PROOF_PIPELINE_FILE=str(pipeline_path), PATH=f"{wrapper_dir}:{os.environ['PATH']}",
                   BUILDKITE_GHA_TELEMETRY_DISABLED="true",
                   BUILDKITE_PLUGIN_CONFIGURATION=json.dumps({"workflow": ".buildkite/fixtures/pinned-runtime-proof.yml", "runners": [{"runs-on": "ubuntu-latest", "queue": "hosted"}]}),
                   BUILDKITE_GHA_PLUGIN_RELEASE=json.dumps({"tag_name": "v0.0.0-proof", "assets": [{"name": "buildkite-gha_linux_x86_64", "browser_download_url": assets[name]["url"], "digest": "sha256:" + assets[name]["sha256"]}]}))
        subprocess.run([str(binary), "plugin"], env=env, check=True)
    pipeline = json.loads(capture(["buildkite-agent", "pipeline", "upload", "--dry-run", "--no-interpolation", "--format", "json", str(pipeline_path)]))
    assert len(pipeline["steps"]) == 1 and len(pipeline["steps"][0]["steps"]) == 1
    step = pipeline["steps"][0]["steps"][0]
    paths = inventory(os.environ["BUILDKITE_JOB_ID"])
    distributions = [path for path in paths if "/distributions/" in path]
    if name == "baseline":
        assert len(distributions) == 1 and "command" in step and "plugins" not in step
    else:
        assert not distributions and step["command"] == ""
        config = step["plugins"][0][PLUGIN]
        assert config["run"]["runtime"]["sha256"] == assets[name]["sha256"]
        assert config["run"]["plan"]["producer"] == os.environ["BUILDKITE_JOB_ID"]
    (OUTPUT / f"{name}-inventory.json").write_text(json.dumps(paths, indent=2))
    upload(OUTPUT / f"{name}-inventory.json")
    step_path = OUTPUT / f"{name}-step.json"
    step_path.write_text(json.dumps(step))
    upload(step_path)
    save_metadata(f"pinned-runtime-proof-{name}", os.environ["BUILDKITE_JOB_ID"])
    print(f"{name}: runtime distribution artifacts={len(distributions)}")
    if name == "candidate":
        # Preserve the actual generated job identity. The outer harness runs
        # each unmodified command/config through agent bootstrap, not hooks.
        del step["plugins"]
        step["command"] = "python3 .buildkite/pinned-runtime-proof.py run"
        step["checkout"]["skip"] = False
        step["timeout_in_minutes"] = 15
        step["retry"] = {"automatic": False}
        subprocess.run(["buildkite-agent", "pipeline", "upload", "--no-interpolation"], input=json.dumps(pipeline), text=True, check=True)


def run_cases():
    assets = metadata("pinned-runtime-proof-assets")
    steps = {}
    for name in ("baseline", "candidate"):
        producer = metadata(f"pinned-runtime-proof-{name}")
        steps[name] = json.loads(download(f".pinned-runtime-proof/{name}-step.json", producer).read_text())
    assert steps["baseline"]["key"] == steps["candidate"]["key"] == os.environ["BUILDKITE_STEP_KEY"]
    real_agent, real_curl = shutil.which("buildkite-agent"), shutil.which("curl")
    print(capture([real_agent, "--version"]))
    OUTPUT.mkdir(exist_ok=True)
    exit_file = Path("/tmp/buildkite-gha-proof-exit")
    results = []
    try:
        with tempfile.TemporaryDirectory(prefix="pinned-execution-") as tmp:
            root = Path(tmp)
            wrapper_dir = root / "bin"
            wrappers(wrapper_dir)
            calls = root / "calls.jsonl"
            calls.touch()
            calls.chmod(0o666)
            env = {key: value for key, value in os.environ.items() if not key.startswith("BUILDKITE_PLUGIN_")}
            env.update(PROOF_REAL_AGENT=real_agent, PROOF_REAL_CURL=real_curl, PROOF_CALLS=str(calls),
                       PROOF_RUNTIME_URL=assets["candidate"]["url"],
                       PATH=f"{wrapper_dir}:{os.environ['PATH']}", BUILDKITE_GHA_CACHE_DIR=str(root / "cache"),
                       BUILDKITE_GHA_TELEMETRY_DISABLED="true", BUILDKITE_PLUGIN_VALIDATION="true")
            bootstrap = [real_agent, "bootstrap", "--skip-checkout", "--plugins-path", str(root / "plugins"),
                         "--sockets-path", str(root / "sockets"), "--bin-path", str(wrapper_dir),
                         "--plugin-validation", "--phases", "plugin,checkout,command"]
            for case, mode, status, downloads in (("cold", "candidate", 0, 1), ("warm-retry-replay", "candidate", 0, 0),
                                                   ("baseline", "baseline", 0, 0), ("baseline-failure", "baseline", 1, 0),
                                                   ("pinned-failure", "candidate", 1, 0), ("bad-digest", "candidate", 1, 1)):
                exit_file.write_text("37" if "failure" in case else "0")
                step = steps[mode]
                jobenv = dict(env, **step["env"])
                jobenv["BUILDKITE_RETRY_COUNT"] = "1" if case == "warm-retry-replay" else "0"
                jobenv["BUILDKITE_COMMAND"] = step.get("command", "")
                config = json.loads(json.dumps(step.get("plugins", [{}])[0].get(PLUGIN, {})))
                if case == "bad-digest":
                    config["run"]["runtime"]["sha256"] = "0" * 64
                jobenv["BUILDKITE_PLUGINS"] = json.dumps([{PLUGIN: config}]) if config else "[]"
                calls.write_text("")
                result = subprocess.run(bootstrap, env=jobenv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
                output = result.stdout
                for name in ("baseline", "candidate"):
                    output = output.replace(assets[name]["url"], "<fixture-runtime-url>")
                (OUTPUT / f"{case}.log").write_text(output)
                print(output)
                observed = [json.loads(line) for line in calls.read_text().splitlines()]
                record = {"case": case, "status": result.returncode, "binary_downloads": observed.count("runtime"),
                          "release_lookups": observed.count("metadata"),
                          "runtime_artifact_downloads": observed.count("distribution")}
                results.append(record)
                print(json.dumps(record))
                assert result.returncode == status, case
                assert (MARKER in output) == (case != "bad-digest"), case
                assert record["binary_downloads"] == downloads and record["release_lookups"] == 0, case
                assert record["runtime_artifact_downloads"] == (1 if mode == "baseline" else 0), case
                if case == "bad-digest":
                    assert "checksum verification failed" in output
    finally:
        exit_file.unlink(missing_ok=True)
        (OUTPUT / "results.json").write_text(json.dumps(results, indent=2))
        for path in list(OUTPUT.glob("*.log")) + [OUTPUT / "results.json"]:
            upload(path)
    print("PASS: real artifacts/backend, normal plugin installation, cold/warm retry-context replay and rejection controls")
    print("LIMIT: fixture release metadata; nested bootstrap; retry replay is not a scheduler retry or cross-worker proof")


def delegate(mode):
    args = sys.argv[2:]
    if mode == "agent" and args[:2] == ["pipeline", "upload"] and os.environ.get("PROOF_PIPELINE_FILE"):
        Path(os.environ["PROOF_PIPELINE_FILE"]).write_text(sys.stdin.read())
        return
    event = None
    if mode == "curl":
        if os.environ["PROOF_RUNTIME_URL"] in args:
            event = "runtime"
        elif any("/releases/" in arg for arg in args):
            event = "metadata"
    elif args[:2] == ["artifact", "download"] and "/distributions/" in args[2]:
        event = "distribution"
    if event and os.environ.get("PROOF_CALLS"):
        with open(os.environ["PROOF_CALLS"], "a") as file:
            file.write(json.dumps(event) + "\n")
    executable = os.environ["PROOF_REAL_AGENT" if mode == "agent" else "PROOF_REAL_CURL"]
    os.execv(executable, [executable, *args])


if __name__ == "__main__":
    if sys.argv[1] == "publish":
        publish()
    elif sys.argv[1] == "import":
        import_workflow(sys.argv[2])
    elif sys.argv[1] == "run":
        run_cases()
    else:
        delegate(sys.argv[1])
