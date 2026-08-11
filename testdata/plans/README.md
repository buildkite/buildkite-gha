# Plan-envelope conformance fixtures

These fixtures exercise the plan-envelope trust contract without production Go code.
They use disposable public test keys that must never be installed as production
trust roots.

Run:

```bash
python3 testdata/plans/validate.py
```

The checker needs Python 3 and OpenSSL 3. It implements only the strict JSON
subset used by this corpus, verifies the ES256 signatures, and applies the ADR's
verification order. It is a conformance aid, not a production JCS, JSON Schema,
or JOSE implementation. Production code must use complete implementations of
RFC 7515 and RFC 8785 and validate against the schemas in `schemas/`.

`cases.json` supplies the runtime-observed build, step, queue, time, and local
capability ceiling. Negative cases remain cryptographically valid unless the
condition under test is the signature or plan digest:

- `tampered` changes the signed plan's command without changing its envelope;
- `wrong-build`, `wrong-job`, and `wrong-queue` replay a valid envelope in the
  wrong runtime context;
- `expired` evaluates a valid envelope after its expiry;
- `untrusted-event` carries a valid signature but exceeds current local policy
  for its signed untrusted provenance; and
- `untrusted-key` is correctly signed by a key absent from the trust bundle.

The canonical plan files are stored as one-line JCS JSON. Envelope files are
pretty-printed because their claims are canonicalized during signing and
verification rather than signing their transport whitespace.
