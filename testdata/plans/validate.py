#!/usr/bin/env python3
"""Dependency-free Plan-envelope fixture checker."""

from __future__ import annotations

import base64
import hashlib
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent
CAPABILITIES = {
    "network",
    "docker",
    "privileged-container",
    "secrets",
    "provider-token-read",
    "provider-token-write",
}
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\Z")
NAME = re.compile(r"[A-Za-z0-9_-]{1,255}\Z")


class Rejection(Exception):
    def __init__(self, code: str):
        self.code = code


def reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load(path: Path):
    with path.open(encoding="utf-8") as handle:
        return json.load(handle, object_pairs_hook=reject_duplicates)


def canonical(value) -> bytes:
    """JCS-equivalent encoder for the fixture corpus's integer-only JSON."""
    if isinstance(value, float):
        raise ValueError("fixture checker deliberately rejects floating-point JSON")
    if isinstance(value, dict):
        body = ",".join(
            json.dumps(key, ensure_ascii=False, separators=(",", ":"))
            + ":"
            + canonical(item).decode("utf-8")
            for key, item in sorted(value.items())
        )
        return ("{" + body + "}").encode()
    if isinstance(value, list):
        return ("[" + ",".join(canonical(item).decode() for item in value) + "]").encode()
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()


def b64url_decode(value: str) -> bytes:
    if "=" in value or not re.fullmatch(r"[A-Za-z0-9_-]+", value):
        raise ValueError("non-canonical base64url")
    return base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))


def der_length(length: int) -> bytes:
    if length < 128:
        return bytes([length])
    encoded = length.to_bytes((length.bit_length() + 7) // 8, "big")
    return bytes([0x80 | len(encoded)]) + encoded


def der(tag: int, value: bytes) -> bytes:
    return bytes([tag]) + der_length(len(value)) + value


def der_integer(value: bytes) -> bytes:
    value = value.lstrip(b"\0") or b"\0"
    if value[0] & 0x80:
        value = b"\0" + value
    return der(0x02, value)


def public_key_der(jwk: dict) -> bytes:
    if (
        set(jwk) != {"alg", "crv", "kid", "key_ops", "kty", "use", "x", "y"}
        or jwk["alg"] != "ES256"
        or jwk["crv"] != "P-256"
        or jwk["kty"] != "EC"
        or jwk["key_ops"] != ["verify"]
        or jwk["use"] != "sig"
    ):
        raise Rejection("E_UNTRUSTED_KEY")
    point = b"\x04" + b64url_decode(jwk["x"]) + b64url_decode(jwk["y"])
    if len(point) != 65:
        raise Rejection("E_UNTRUSTED_KEY")
    algorithm = der(
        0x30,
        der(0x06, bytes.fromhex("2a8648ce3d0201"))
        + der(0x06, bytes.fromhex("2a8648ce3d030107")),
    )
    return der(0x30, algorithm + der(0x03, b"\0" + point))


def require_keys(value: dict, expected: set[str], code: str):
    if not isinstance(value, dict) or set(value) != expected:
        raise Rejection(code)


def validate_structure(envelope: dict, plan: dict) -> dict:
    require_keys(envelope, {"claims", "protected", "signature"}, "E_SCHEMA")
    if not isinstance(envelope["protected"], str) or not isinstance(envelope["signature"], str):
        raise Rejection("E_SCHEMA")
    claims = envelope["claims"]
    require_keys(
        claims,
        {"iss", "jti", "iat", "exp", "plan", "build", "provenance", "target", "authorization"},
        "E_SCHEMA",
    )
    if claims["iss"] != "buildkite-gha-plan-envelope" or not UUID.fullmatch(claims["jti"]):
        raise Rejection("E_SCHEMA")
    if type(claims["iat"]) is not int or type(claims["exp"]) is not int or claims["iat"] < 0 or claims["exp"] < 0:
        raise Rejection("E_SCHEMA")
    require_keys(claims["plan"], {"digest", "schema", "compiler_version", "compiler_distribution_digest"}, "E_SCHEMA")
    require_keys(claims["build"], {"organization_id", "pipeline_id", "build_id"}, "E_SCHEMA")
    require_keys(claims["target"], {"step_key", "queue"}, "E_SCHEMA")
    require_keys(claims["authorization"], {"capability_ceiling"}, "E_SCHEMA")
    for value in claims["build"].values():
        if not UUID.fullmatch(value):
            raise Rejection("E_SCHEMA")
    if not NAME.fullmatch(claims["target"]["step_key"]) or not NAME.fullmatch(claims["target"]["queue"]):
        raise Rejection("E_SCHEMA")
    for field in ("digest", "compiler_distribution_digest"):
        if not DIGEST.fullmatch(claims["plan"][field]):
            raise Rejection("E_SCHEMA")
    capabilities = claims["authorization"]["capability_ceiling"]
    if capabilities != sorted(set(capabilities)) or not set(capabilities) <= CAPABILITIES:
        raise Rejection("E_SCHEMA")

    require_keys(plan, {"schema", "compiler", "workflow", "event", "target", "required_capabilities", "steps"}, "E_PLAN_SCHEMA")
    require_keys(plan["compiler"], {"version", "distribution_digest"}, "E_PLAN_SCHEMA")
    require_keys(plan["workflow"], {"path", "digest", "logical_job_id"}, "E_PLAN_SCHEMA")
    require_keys(plan["event"], {"provider", "name", "payload_digest"}, "E_PLAN_SCHEMA")
    require_keys(plan["target"], {"step_key", "queue"}, "E_PLAN_SCHEMA")
    required = plan["required_capabilities"]
    if required != sorted(set(required)) or not set(required) <= CAPABILITIES:
        raise Rejection("E_PLAN_SCHEMA")
    return claims


def verify_signature(envelope: dict, trust_roots: dict, revoked: set[str]) -> dict:
    try:
        protected_raw = b64url_decode(envelope["protected"])
        protected = json.loads(protected_raw, object_pairs_hook=reject_duplicates)
    except (ValueError, json.JSONDecodeError):
        raise Rejection("E_PROTECTED_HEADER")
    require_keys(protected, {"alg", "kid", "typ"}, "E_PROTECTED_HEADER")
    if protected["alg"] != "ES256" or protected["typ"] != "buildkite-gha-plan-envelope+jws":
        raise Rejection("E_PROTECTED_HEADER")
    if protected_raw != canonical(protected):
        raise Rejection("E_PROTECTED_HEADER")
    if protected["kid"] in revoked:
        raise Rejection("E_REVOKED_KEY")
    keys = [key for key in trust_roots.get("keys", []) if key.get("kid") == protected["kid"]]
    if len(keys) != 1:
        raise Rejection("E_UNTRUSTED_KEY")
    try:
        raw_signature = b64url_decode(envelope["signature"])
    except ValueError:
        raise Rejection("E_SIGNATURE")
    if len(raw_signature) != 64:
        raise Rejection("E_SIGNATURE")
    signature = der(0x30, der_integer(raw_signature[:32]) + der_integer(raw_signature[32:]))
    signing_input = envelope["protected"].encode() + b"." + base64.urlsafe_b64encode(canonical(envelope["claims"])).rstrip(b"=")
    with tempfile.TemporaryDirectory() as directory:
        directory = Path(directory)
        public = directory / "public.der"
        message = directory / "message"
        sig = directory / "signature.der"
        public.write_bytes(public_key_der(keys[0]))
        message.write_bytes(signing_input)
        sig.write_bytes(signature)
        result = subprocess.run(
            ["openssl", "dgst", "-sha256", "-verify", str(public), "-keyform", "DER", "-signature", str(sig), str(message)],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
    if result.returncode != 0:
        raise Rejection("E_SIGNATURE")
    return protected


def verify_case(case: dict, trust_roots: dict, revoked: set[str]) -> str:
    try:
        envelope = load(ROOT / case["envelope"])
        plan = load(ROOT / case["plan"])
        claims = validate_structure(envelope, plan)
        verify_signature(envelope, trust_roots, revoked)

        runtime = case["runtime"]
        if not (
            claims["iat"] <= runtime["now"] < claims["exp"]
            and claims["exp"] > claims["iat"]
            and claims["exp"] - claims["iat"] <= 86400
        ):
            raise Rejection("E_EXPIRED")
        if any(claims["build"][field] != runtime[field] for field in ("organization_id", "pipeline_id", "build_id")):
            raise Rejection("E_BUILD_BINDING")
        if claims["target"]["step_key"] != runtime["step_key"]:
            raise Rejection("E_STEP_BINDING")
        if claims["target"]["queue"] != runtime["queue"]:
            raise Rejection("E_QUEUE_BINDING")
        ceiling = set(claims["authorization"]["capability_ceiling"])
        local_ceiling = set(runtime["local_capability_ceiling"])
        if not ceiling <= local_ceiling:
            raise Rejection("E_CAPABILITY_POLICY")

        digest = "sha256:" + hashlib.sha256(canonical(plan)).hexdigest()
        if digest != claims["plan"]["digest"]:
            raise Rejection("E_PLAN_DIGEST")
        if (
            plan["schema"] != claims["plan"]["schema"]
            or plan["compiler"]["version"] != claims["plan"]["compiler_version"]
            or plan["compiler"]["distribution_digest"] != claims["plan"]["compiler_distribution_digest"]
            or plan["workflow"]["path"] != claims["provenance"]["workflow_path"]
            or plan["workflow"]["digest"] != claims["provenance"]["workflow_digest"]
            or plan["event"]["provider"] != claims["provenance"]["provider"]
            or plan["event"]["name"] != claims["provenance"]["event_name"]
            or plan["event"]["payload_digest"] != claims["provenance"]["event_payload_digest"]
            or plan["target"] != claims["target"]
        ):
            raise Rejection("E_PLAN_BINDING")
        if not set(plan["required_capabilities"]) <= ceiling & local_ceiling:
            raise Rejection("E_CAPABILITY_REQUEST")
        return "OK"
    except Rejection as rejection:
        return rejection.code
    except (KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
        return "E_SCHEMA"


def main() -> int:
    cases = load(ROOT / "cases.json")
    trust_roots = load(ROOT / "trust-roots.jwks.json")
    revoked = set(load(ROOT / "revoked-kids.json"))
    failures = 0
    for case in cases:
        actual = verify_case(case, trust_roots, revoked)
        state = "PASS" if actual == case["expected"] else "FAIL"
        print(f"{state} {case['name']}: {actual}")
        failures += state == "FAIL"
    print(f"{len(cases) - failures}/{len(cases)} fixtures matched expected outcomes")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
