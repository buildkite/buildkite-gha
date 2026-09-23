#!/usr/bin/env python3
"""Temporary production proof: record non-secret API bodies and uploaded YAML."""
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


def native(response):
    target = response.get("resolutions", [{}])[0].get("target", {})
    return (target.get("queue") == "linux-medium"
            and target.get("platform") == "linux/amd64"
            and target.get("agents") == {"nsc-gha-image": "ubuntu-24.04"}
            and target.get("tool_cache") is False and "image" not in target)


if len(sys.argv) > 1 and sys.argv[1] == "--agent":
    args = sys.argv[2:]
    env = dict(os.environ, BUILDKITE_AGENT_ENDPOINT=os.environ["PB3064_UPSTREAM"])
    data = None
    if args[:2] == ["pipeline", "upload"]:
        data = sys.stdin.buffer.read()
        pipeline = data.decode()
        Path(".pb3064-evidence/pipeline.yml").write_bytes(data)
        assert re.search(r'(?m)^\s+queue: "linux-medium"$', pipeline), pipeline
        assert re.search(r'(?m)^\s+"nsc-gha-image": "ubuntu-24.04"$', pipeline), pipeline
        assert not re.search(r'(?m)^\s+image:', pipeline), pipeline
        assert "--hosted-tool-cache" not in pipeline, pipeline
        assert "/opt/hostedtoolcache" not in pipeline, pipeline
        print("PB3064 generated pipeline PASS: queue=linux-medium nsc-gha-image=ubuntu-24.04; no step image or hosted-tool-cache bootstrap", file=sys.stderr, flush=True)
        for line in pipeline.splitlines():
            if re.match(r'^\s+(agents:|queue:|"nsc-gha-image":)', line):
                print(line, file=sys.stderr, flush=True)
    result = subprocess.run([os.environ["PB3064_REAL_AGENT"], *args], input=data, env=env)
    sys.exit(result.returncode)


evidence = Path(".pb3064-evidence")
evidence.mkdir(exist_ok=True)
upstream = os.environ["BUILDKITE_AGENT_ENDPOINT"].rstrip("/")
agent = shutil.which("buildkite-agent")
request_body = {"supports_agent_tags": True, "requirements": [{"id": "r1", "selector": {"labels": ["ubuntu-24.04"]}}]}
url = upstream + "/jobs/" + os.environ["BUILDKITE_JOB_ID"] + "/github-actions/runners"
headers = {"Authorization": "Token " + os.environ["BUILDKITE_AGENT_ACCESS_TOKEN"], "Content-Type": "application/json", "Accept": "application/json"}
print("PB3064 production endpoint:", url, flush=True)
for attempt in range(12):
    request = urllib.request.Request(url, data=json.dumps(request_body).encode(), headers=headers)
    with urllib.request.urlopen(request, timeout=30) as response:
        body = json.load(response)
    print("PB3064 preflight", attempt + 1, json.dumps(body), flush=True)
    if native(body):
        print("PB3064 Namespace registration gate PASS: production native response requires namespace_queue_id.present?", flush=True)
        break
    if any(r.get("error") for r in body.get("resolutions", [])):
        raise SystemExit("PB3064 registration/queue resolution failed; see preflight response")
    time.sleep(60)
else:
    raise SystemExit("PB3064 production native target unavailable after 12 checks at 60-second cadence")


class Recorder(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # Never log authorization headers or unrelated API responses.

    def do_POST(self):
        data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        origin = urllib.parse.urlsplit(upstream)
        target = urllib.parse.urlunsplit((origin.scheme, origin.netloc, self.path, "", ""))
        forwarded = {key: value for key, value in self.headers.items() if key.lower() not in ("host", "content-length", "connection", "accept-encoding")}
        req = urllib.request.Request(target, data=data, headers=forwarded)
        try:
            response = urllib.request.urlopen(req, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            output = response.read()
            status = response.status
        if self.path.endswith("/github-actions/runners"):
            sent, received = json.loads(data), json.loads(output)
            (evidence / "cli-request.json").write_text(json.dumps(sent, indent=2))
            (evidence / "production-response.json").write_text(json.dumps(received, indent=2))
            print("PB3064 actual CLI request:", json.dumps(sent), flush=True)
            print("PB3064 actual production response:", status, json.dumps(received), flush=True)
            assert sent["supports_agent_tags"] is True
            assert status == 200 and native(received)
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(output)))
        self.end_headers()
        self.wfile.write(output)


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Recorder)
threading.Thread(target=server.serve_forever, daemon=True).start()
with tempfile.TemporaryDirectory() as wrappers:
    wrapper = Path(wrappers) / "buildkite-agent"
    wrapper.write_text('#!/bin/sh\nexec python3 "$PB3064_RECORDER" --agent "$@"\n')
    wrapper.chmod(0o755)
    env = dict(os.environ, PB3064_UPSTREAM=upstream, PB3064_REAL_AGENT=agent,
               PB3064_RECORDER=str(Path(__file__).resolve()),
               BUILDKITE_AGENT_ENDPOINT=f"http://127.0.0.1:{server.server_port}" + urllib.parse.urlsplit(upstream).path,
               PATH=wrappers + ":" + os.environ["PATH"])
    event = json.loads(Path("testdata/smoke/events/push.json").read_text())
    event.update(event="workflow_dispatch", sha=os.environ["BUILDKITE_COMMIT"], ref="refs/heads/" + os.environ["BUILDKITE_BRANCH"])
    (evidence / "event.json").write_text(json.dumps(event))
    result = subprocess.run([sys.argv[1], "upload", "--event-path", str(evidence / "event.json"), ".github/workflows/pb3064-native-smoke.yml"], env=env)
    server.shutdown()
    subprocess.run([agent, "artifact", "upload", ".pb3064-evidence/*"], check=True)
    sys.exit(result.returncode)
