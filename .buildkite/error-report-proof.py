"""Run a digest-pinned CLI in a real job; never print tokens or report bodies."""

import argparse
from datetime import datetime, timezone
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import urllib.parse


parser = argparse.ArgumentParser()
parser.add_argument("--binary", required=True)
parser.add_argument("--sha256", required=True)
parser.add_argument("--version", required=True)
args = parser.parse_args()
binary = Path(args.binary).resolve()
assert hashlib.sha256(binary.read_bytes()).hexdigest() == args.sha256
version = subprocess.check_output([str(binary), "--version"], text=True).strip()
assert args.version.removeprefix("v") in version
for key in ("BUILDKITE_JOB_ID", "BUILDKITE_AGENT_ENDPOINT", "BUILDKITE_AGENT_ACCESS_TOKEN"):
    assert os.environ.get(key), f"Missing real-job variable: {key}"
endpoint = urllib.parse.urlsplit(os.environ["BUILDKITE_AGENT_ENDPOINT"])
assert endpoint.scheme in ("http", "https") and endpoint.hostname
token = os.environ["BUILDKITE_AGENT_ACCESS_TOKEN"]
reports = []
failures = []


class Receiver(http.server.BaseHTTPRequestHandler):
    def log_message(self, *unused):
        pass

    def do_POST(self):
        try:
            body = self.rfile.read(int(self.headers["Content-Length"]))
            is_report = self.path.endswith("/github-actions/error-reports")
            if is_report:
                payload = json.loads(body)
                assert "apiKey" not in payload
                assert len(payload["events"]) == 1 and len(body) <= 512 * 1024
                assert isinstance(payload["notifier"], dict)
                event = payload["events"][0]
                assert event["payloadVersion"] == "4"
                assert event["context"] == "plugin_import:artifact_upload"
                assert event["app"]["version"] == args.version.removeprefix("v")
                assert token.encode() not in body and b"synthetic-private-marker" not in body
                for exception in event["exceptions"]:
                    assert exception["message"] == "buildkite-gha plugin_import failed during artifact_upload"
                    for frame in exception["stacktrace"]:
                        assert frame["file"] == "[REDACTED]"
                assert self.headers["Authorization"] == "Token " + token
                assert not any(key.lower().startswith("bugsnag-") for key in self.headers)
            connection_type = http.client.HTTPSConnection if endpoint.scheme == "https" else http.client.HTTPConnection
            connection = connection_type(endpoint.hostname, endpoint.port, timeout=5)
            report_time = datetime.now(timezone.utc).isoformat()
            try:
                connection.request("POST", self.path, body, {
                    "Authorization": self.headers["Authorization"],
                    "Content-Type": "application/json",
                    "User-Agent": self.headers.get("User-Agent", ""),
                })
                response = connection.getresponse()
                status = response.status
                response.read(32768)
            finally:
                connection.close()
            if is_report:
                reports.append({"status": status, "bytes": len(body), "sha256": hashlib.sha256(body).hexdigest(),
                                "utc": report_time, "app_version": event["app"]["version"],
                                "context": event["context"],
                                "error_classes": [item["errorClass"] for item in event["exceptions"]]})
            self.send_response(status)
            self.end_headers()
        except Exception as error:
            # Exception text may contain response data or credentials.
            failures.append(type(error).__name__)
            self.send_response(502)
            self.end_headers()


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
scratch = Path(".amp/in/orb-backup")
scratch.mkdir(parents=True, exist_ok=True)
with tempfile.TemporaryDirectory(prefix="gha-error-report-proof-", dir=scratch) as temporary:
    temporary = str(Path(temporary).resolve())
    agent = Path(temporary) / "buildkite-agent"
    agent.write_text("#!/bin/sh\necho 'synthetic artifact failure synthetic-private-marker' >&2\nexit 1\n")
    agent.chmod(0o755)
    env = dict(os.environ,
               BUILDKITE_AGENT_ENDPOINT=f"http://127.0.0.1:{server.server_port}{endpoint.path.rstrip('/')}",
               BUILDKITE_GHA_TELEMETRY_DISABLED="",
               PATH=temporary + os.pathsep + os.environ["PATH"])
    env.pop("BUILDKITE_GHA_PLUGIN_RELEASE", None)
    started = time.monotonic()
    result = subprocess.run([
        str(binary), "upload", "testdata/smoke/.github/workflows/shell.yml",
        "--event-path", "testdata/smoke/events/push.json", "--runtime-queue", "hosted",
    ], env=env, capture_output=True, timeout=60)
    elapsed = time.monotonic() - started
server.shutdown()
server.server_close()
thread.join()
assert result.returncode == 1, f"Unexpected CLI exit: {result.returncode}"
assert not failures, failures
assert len(reports) == 1, f"Expected one error-report request, got {len(reports)}"
assert reports[0]["status"] == 202, f"Agent API status: {reports[0]['status']}"
print(json.dumps({"version": version, "binary_sha256": args.sha256,
                  "exit": result.returncode, "elapsed_seconds": round(elapsed, 3),
                  "sanitized": True, "reports": reports,
                  "delivery": "queued; confirm worker and Bugsnag separately"}, sort_keys=True))
