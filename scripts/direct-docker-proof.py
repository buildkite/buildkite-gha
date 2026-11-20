#!/usr/bin/env python3
"""Generate portable workflows for native and hosted direct-image acceptance."""

import json
from pathlib import Path
import sys


PYTHON = "python@sha256:c5ba3deef00c7758561a0f03c26f44f10589263d40e732b8748e0d1f38f6dd89"
NONROOT = "nginxinc/nginx-unprivileged@sha256:f5116ed4606f3eb015dd4a8e28cb9b351476dc0ad4bc6e9f93b19cec130cdf03"
IMAGE_PATH = "/usr/local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Observed on the native runner; expectations do not use our argument parser.
ARGV = [
    ('left a"b c" right', ["left", "ab c", "right"]),
    ('left "a"b" right', ["left", "ab right"]),
    (r'left a\"b c" right', ["left", 'a"b', "c right"]),
    (r'left "a\"b" right', ["left", 'a"b', "right"]),
    (r'left a\\"b c" right', ["left", r"a\b c", "right"]),
    (r'left "a\\"b" right', ["left", r"a\b right"]),
    (r'left a\\\"b c" right', ["left", r'a\"b', "c right"]),
    (r'left "a\\\"b" right', ["left", r'a\"b', "right"]),
    (r'left a\\\\"b c" right', ["left", r"a\\b c", "right"]),
    (r'left "a\\\\"b" right', ["left", r"a\\b right"]),
    ('left """" right', ["left", '"', "right"]),
    ('left a""b right', ["left", "ab", "right"]),
    ('left "a""b" right', ["left", 'a"b', "right"]),
    ('left "a"""b" right', ["left", 'a"b right']),
    ('left "" "" right', ["left", "", "", "right"]),
    ('left pre""""post right', ["left", 'pre"post', "right"]),
    ("left 'two words' right", ["left", "'two", "words'", "right"]),
    (r'$HOME ${HOME} $(id) `id` ; && || | > < * ? ~',
     ["$HOME", "${HOME}", "$(id)", "`id`", ";", "&&", "||", "|", ">", "<", "*", "?", "~"]),
]

PROBE = r'''
import json, os, pathlib, shutil, socket, sys
mode = sys.argv[1]
record = {"mode": mode, "argv": sys.argv[2:], "home": os.environ["HOME"],
          "cwd": os.getcwd(), "uid": os.getuid(), "gid": os.getgid(),
          "path": os.environ["PATH"], "tool": shutil.which("direct-tool"),
          "docker_socket": pathlib.Path("/var/run/docker.sock").is_socket()}
assert record["home"] == "/github/home", record
assert record["cwd"] == "/github/workspace", record
assert record["uid"] == 0, record
if mode.startswith("argv-"):
    assert record["argv"] == EXPECTED_ARGV[int(mode[5:])], record
else:
    aliases = {"ALIAS_WS": "/github/workspace", "ALIAS_FILE": "/github/workspace/fixture.txt",
               "ALIAS_TEMP": "/github/runner_temp", "ALIAS_TEMP_FILE": "/github/runner_temp/proof.txt",
               "INPUT_DIRECTORY": "/github/workspace", "INPUT_FILE": "/github/workspace/fixture.txt",
               "INPUT_TEMP": "/github/runner_temp", "INPUT_TEMP_FILE": "/github/runner_temp/proof.txt"}
    record["aliases"] = {key: os.environ[key] for key in aliases}
    assert record["aliases"] == aliases, record
    for key in ("ALIAS_FILE", "INPUT_FILE"):
        assert pathlib.Path(os.environ[key]).read_text() == "workspace-marker", record
    for key in ("ALIAS_TEMP_FILE", "INPUT_TEMP_FILE"):
        assert pathlib.Path(os.environ[key]).read_text() == "temp-marker", record
    original = os.environ["ORIGINAL_WORKSPACE"][len("prefix:"):]
    assert os.environ["EMBEDDED"] == "prefix:" + original + "/fixture.txt", record
    assert os.environ["NEAR_PREFIX"] == original + "-suffix", record
    assert os.environ["INPUT_MIXED-KEY"] == "two words", record
    assert os.environ["INPUT_EMPTY"] == "", record
    assert os.environ["INPUT_WHITESPACE"] == "   ", record
    expected_path = os.environ["EXPECTED_PATH"]
    if mode == "nonleading":
        expected_path = "/usr/local/bin:" + original + "/proof-bin:/usr/bin:/bin"
    assert record["path"] == expected_path, record
    if mode == "step-path":
        assert record["tool"] == "/github/workspace/proof-bin/direct-tool", record
    else:
        assert record["tool"] is None, record
    home = pathlib.Path(os.environ["HOME"]) / "direct-home-marker"
    if mode == "first":
        assert not home.exists(), record
        if os.environ["SIBLING"] == "true":
            assert pathlib.Path("/github/home/parent-home-marker").read_text() == "parent-marker", record
        home.write_text("shared-marker")
        with socket.create_connection(("cache", 6379), timeout=5) as service:
            service.sendall(b"PING\r\n")
            assert service.recv(64) == b"+PONG\r\n", record
        with open(os.environ["GITHUB_OUTPUT"], "a") as out:
            out.write("result=direct-proof\n")
        with open(os.environ["GITHUB_ENV"], "a") as out:
            out.write("DIRECT_ENV=visible\n")
        with open(os.environ["GITHUB_PATH"], "a") as out:
            out.write("/synthetic/action-added\n")
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as out:
            out.write("direct Docker proof summary\n")
        print("::add-mask::synthetic-direct-proof-canary")
        print("synthetic-direct-proof-canary")
    else:
        assert home.read_text() == "shared-marker", record
print("DIRECT_DOCKER_RECORD=" + json.dumps(record, sort_keys=True))
'''


def prepare(sibling):
    probe = "EXPECTED_ARGV = " + repr([want for _, want in ARGV]) + "\n" + PROBE
    return {"name": "Prepare files and parent controls", "shell": "sh", "run": (
        "set -eu\n"
        "printf workspace-marker > fixture.txt\n"
        "printf temp-marker > \"$RUNNER_TEMP/proof.txt\"\n"
        "mkdir -p proof-bin\n"
        "printf '#!/bin/sh\\nprintf tool-marker\\n' > proof-bin/direct-tool\n"
        "chmod 755 proof-bin/direct-tool\n"
        "printf '%s\\n' \"$GITHUB_WORKSPACE/proof-bin\" /synthetic/added-bin >> \"$GITHUB_PATH\"\n"
        + ("test \"$HOME\" = /github/home\nprintf parent-marker > \"$HOME/parent-home-marker\"\n" if sibling else "")
        + "cat > proof.py <<'PY'\n" + probe + "\nPY\n"
        + "cat > empty.sh <<'SH'\n#!/bin/sh\nset -eu\ntest \"$#\" -eq 0\nprintf 'DIRECT_DOCKER_DEFAULT=entrypoint-clears-CMD\\n'\nSH\n"
        + "cat > nonroot.sh <<'SH'\nset -eu\ntest \"$(id -u)\" = 101\ntest \"$(id -g)\" = 101\ntest \"$HOME\" = /github/home\ntest \"$(cat \"$HOME/direct-home-marker\")\" = shared-marker\nprintf 'DIRECT_DOCKER_USER={\"uid\":101,\"gid\":101,\"home\":\"/github/home\"}\\n'\nSH\n"
        + "chmod 755 empty.sh\n"
    )}


def environment_step(mode, path):
    return {"name": mode, "uses": "docker://" + PYTHON,
            "env": {"HOME": "/synthetic/caller-home", "EXPECTED_PATH": path,
                    "ALIAS_WS": "${{ github.workspace }}", "ALIAS_FILE": "${{ github.workspace }}/fixture.txt",
                    "ALIAS_TEMP": "${{ runner.temp }}", "ALIAS_TEMP_FILE": "${{ runner.temp }}/proof.txt",
                    "ORIGINAL_WORKSPACE": "prefix:${{ github.workspace }}",
                    "EMBEDDED": "prefix:${{ github.workspace }}/fixture.txt", "NEAR_PREFIX": "${{ github.workspace }}-suffix"},
            "with": {"entrypoint": "/usr/local/bin/python3", "args": "proof.py " + mode,
                     "directory": "${{ github.workspace }}", "file": "${{ github.workspace }}/fixture.txt",
                     "temp": "${{ runner.temp }}", "temp_file": "${{ runner.temp }}/proof.txt",
                     "MiXeD-Key": "two words", "empty": "", "whitespace": "   "}}


def job(sibling=False, initial_path=IMAGE_PATH, quote_cases=True):
    first = environment_step("first", initial_path)
    first["id"] = "first"
    step_path = environment_step("step-path", "/github/workspace/proof-bin:/usr/local/bin:/usr/bin:/bin")
    step_path["env"]["PATH"] = "${{ github.workspace }}/proof-bin:/usr/local/bin:/usr/bin:/bin"
    nonleading = environment_step("nonleading", "unused")
    nonleading["env"]["PATH"] = "/usr/local/bin:${{ github.workspace }}/proof-bin:/usr/bin:/bin"
    steps = [prepare(sibling), first, environment_step("shared-home", initial_path), step_path, nonleading]
    if quote_cases:
        steps += [{"name": "argv-" + str(i), "uses": "docker://" + PYTHON,
                   "with": {"entrypoint": "/usr/local/bin/python3", "args": "proof.py argv-" + str(i) + " " + args}}
                  for i, (args, _) in enumerate(ARGV)]
        steps += [{"name": "Image USER", "uses": "docker://" + NONROOT,
                   "with": {"entrypoint": "/bin/sh", "args": "nonroot.sh"}},
                  {"name": "Entrypoint clears image CMD", "uses": "docker://" + PYTHON,
                   "with": {"entrypoint": "/github/workspace/empty.sh"}},
                  {"name": "Retain image ENTRYPOINT and CMD", "uses": "docker://alpine/git:2.47.2"},
                  {"name": "Empty args retain CMD", "uses": "docker://alpine/git:2.47.2", "with": {"args": ""}},
                  {"name": "Whitespace args retain CMD", "uses": "docker://alpine/git:2.47.2", "with": {"args": "   "}},
                  {"name": "Args replace CMD", "uses": "docker://alpine/git:2.47.2", "with": {"entrypoint": "", "args": "--version"}}]
    steps += [{"name": "Write PATH after prepends", "shell": "sh", "run": "echo PATH=/synthetic/github-env-bin:/usr/local/bin:/usr/bin:/bin >> \"$GITHUB_ENV\""},
              environment_step("file-path", "/synthetic/github-env-bin:/usr/local/bin:/usr/bin:/bin"),
              {"name": "Verify parent file commands", "shell": "sh", "env": {"RESULT": "${{ steps.first.outputs.result }}"},
               "run": "set -eu\ntest \"$DIRECT_ENV\" = visible\ntest \"$RESULT\" = direct-proof\ntest \"$(command -v direct-tool)\" = \"$GITHUB_WORKSPACE/proof-bin/direct-tool\"\n"
                      + ("test \"$(cat \"$HOME/direct-home-marker\")\" = shared-marker\n" if sibling else "")
                      + "printf 'DIRECT_DOCKER_PARENT={\"outputs\":\"passed\",\"env\":\"passed\",\"path\":\"passed\"}\\n'\n"}]
    result = {"runs-on": "ubuntu-latest", "timeout-minutes": 10,
              "env": {"SIBLING": str(sibling).lower()},
              "services": {"cache": {"image": "redis:7-alpine", "options": '--health-cmd "redis-cli ping" --health-interval 1s --health-timeout 2s --health-retries 10'}},
              "steps": steps}
    if sibling:
        result["container"] = PYTHON
    return result


def main():
    root = Path(sys.argv[1])
    root.mkdir(parents=True, exist_ok=True)
    workflow = {"name": "Direct Docker runtime proof", "on": {"workflow_dispatch": {}}, "permissions": {},
                "jobs": {"direct-host": job(), "direct-sibling": job(sibling=True)}}
    workflow_path = "/synthetic/workflow-bin:/usr/local/bin:/usr/bin:/bin"
    job_path = "/synthetic/job-bin:/usr/local/bin:/usr/bin:/bin"
    paths = {"name": "Direct Docker PATH sources", "on": {"workflow_dispatch": {}}, "permissions": {},
             "env": {"PATH": workflow_path},
             "jobs": {"workflow-path": job(initial_path=workflow_path, quote_cases=False),
                      "job-path": job(initial_path=job_path, quote_cases=False)}}
    paths["jobs"]["job-path"]["env"]["PATH"] = job_path
    for name, value in (("direct-docker.yml", workflow), ("direct-docker-paths.yml", paths)):
        (root / name).write_text(json.dumps(value, indent=2) + "\n")


if __name__ == "__main__":
    main()
