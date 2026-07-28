# GitHub Actions cache compatibility service

Status: **In progress — local adapter and experimental persistence implemented**
Date: 2026-07-27
Parent plan: [`buildkite-gha`: GitHub Actions compatibility for Buildkite](./2026-07-22-buildkite-gha.md)
Backend decision: **Independent GHA compatibility backend in `buildkite/buildkite`; Cache v2 is not a dependency**

## Summary

Make cache-dependent GitHub Actions workflows run unchanged under
`buildkite-gha` by providing the GitHub Actions cache v1 HTTP contract from a
job-local adapter. The adapter presents the protocol expected by
`@actions/cache`, while a pluggable backend stores metadata and opaque cache
archives using a Buildkite-owned cache service.

The first production implementation should:

- start one cache adapter inside each `buildkite-gha run-job` process;
- provision it for every admitted job containing Actions, because cache usage
  is frequently transitive and cannot be inferred reliably from action names;
- inject only `ACTIONS_CACHE_URL` and a random, job-scoped
  `ACTIONS_RUNTIME_TOKEN` into action invocations;
- deliberately leave `ACTIONS_CACHE_SERVICE_V2` and `ACTIONS_RESULTS_URL`
  unset so current cache clients select the v1 protocol;
- keep the adapter alive through JavaScript post-actions, where cache saves
  normally occur;
- derive storage namespace, read scopes, and write scope from trusted
  Buildkite identity and server-verified provider provenance rather than
  workflow-controlled environment or plan fields; and
- call a dedicated, job-authenticated Agent API whose server owns ref
  visibility, reservation, immutable publication, quota, retention, and direct
  blob-transfer instructions.

The backend choice is settled: build the GHA compatibility backend directly in
`buildkite/buildkite`, independently of Cache v2. The two products have
different lookup, visibility, reservation, immutability, and lifecycle
semantics; adapting Cache v2 would couple this work to changes in another
pre-preview interface without reducing the required coordination service.

An experimental directory backend is implemented in `buildkite-gha` to unblock
real client and Hosted integration work. It stores opaque archives on the
existing named Hosted cache volume and is intentionally best effort: volume
snapshot forks do not merge, reservations coordinate only one process, and the
volume is not an authorization boundary. It must not become the production
metadata service or be described as providing concurrent first-writer-wins
semantics.

This remains a bounded delivery slice from Phase 6 of the parent plan. The
production backend derives visibility from authenticated Buildkite job,
pipeline, and webhook-backed provider records. It does not depend on the future
GitHub REST/GraphQL proxy. GitHub artifact v4/results compatibility, provider
API tokens, protected secrets, and private checkout/actions remain separate
later work.

## Current implementation status

As of 2026-07-28:

- the semantic backend interface, cache v1 adapter, bounded sparse PATCH
  assembly, ranges, random job-local bearer, action-only environment, post
  lifecycle, cancellation, and host/container routing are implemented in this
  draft branch;
- a bounded experimental directory backend is wired to generated Hosted action
  jobs through `BUILDKITE_GHA_CACHE_DIR` and the existing `buildkite-gha` named
  cache volume;
- the real pinned `actions/cache@v4` bundle has passed a local miss/save,
  backend restart, and exact-byte restore test over that directory backend;
- `actions/cache` is admitted by the Hosted tokenless profile while artifact
  actions remain rejected;
- exact-commit Hosted builds 205, 206, and 207 each mounted the named volume,
  ran the real v4 action over the v1 adapter, missed, created the deterministic
  payload, and saved successfully, but every later job received an empty volume
  rather than a committed parent; this proves the per-job integration but not
  cross-build persistence on the current Namespace-backed queue;
- exact-commit Hosted build 212 ran the current `lox/notion-cli` workflow
  unchanged, fetched its pinned public commit, ran the default cache-enabled
  `jdx/mise-action@v2` over the v1 adapter, missed and saved a 116,467,256-byte
  archive, then the workflow's `mise run test` and `mise run lint` passed; this
  proves the transitive miss/save path but not a later restore;
- the independent Rails backend and seven job-authenticated Agent API endpoints
  are review-ready at commit `bf9475e3e1d7d22a1149f733d1467877d7352f43`
  in [`buildkite/buildkite` PR
  #31646](https://github.com/buildkite/buildkite/pull/31646): all 226 executed
  CI jobs and Buildsworth review build 9021 passed, but human review, merge,
  deployment, migration, and feature activation remain open; and
- the `buildkite-gha` remote backend client is implemented behind the explicit
  `BUILDKITE_GHA_CACHE_BACKEND=agent` selector but generated jobs still select
  the directory bridge. Production object-store/IAM confirmation, GC
  deployment, policy-value approval, and production canaries remain open.

## User outcome

The production outcome is that users do not have to identify or disable
transitive cache use. These should work without workflow edits once the durable
backend is deployed and selected:

- `actions/cache@v3` and `actions/cache@v4` restore and save;
- setup actions such as `actions/setup-node` and `actions/setup-go` with their
  cache inputs enabled;
- actions that call the public `@actions/cache` package internally; and
- the current `lox/notion-cli` CI workflow with `jdx/mise-action@v2` caching
  left at its default. The current default branch does not contain a
  `cache: false` workaround.

Cache misses, evictions, and save contention remain normal non-fatal cache
outcomes. A cache hit is an accelerator and never an authority for executable
identity, action source integrity, plan integrity, or protected credentials.

## Why action-name special-casing is insufficient

The hosted-tokenless admission check previously rejected a resolved
`actions/cache` action and now admits it onto the experimental backend. Neither
state describes all cache users:

- `jdx/mise-action` can invoke `@actions/cache` from its bundled JavaScript;
- setup actions provide cache options without containing an
  `actions/cache` step in the workflow;
- local, composite, and third-party actions can use the toolkit package; and
- scanning bundled JavaScript is incomplete, brittle, and would turn
  implementation details into policy.

Therefore the runtime should provision cache as a baseline service for every
admitted Actions job. It must not scan action repository names, refs, metadata,
or JavaScript bundles to decide whether to start the service. The explicit
artifact-service rejection remains until the artifact adapter exists.

## Goals

- Support the public cache v1 wire contract used by current `@actions/cache`
  clients.
- Make direct and transitive cache use work without action-specific runtime
  code.
- Preserve exact cache `version` matching and GitHub-compatible ordered key
  fallback.
- Support immutable cache entries, concurrent writers, parallel chunk uploads,
  client retries, and post-action saves safely.
- Persist entries across Buildkite jobs and builds within an explicitly allowed
  pipeline/ref scope.
- Isolate organizations, clusters, pipelines, normal branches, pull requests,
  forks, and tags according to a documented server-side policy.
- Give action code only a job-scoped cache capability, never the Buildkite
  Agent/backend authority used by the parent runtime.
- Work for host JavaScript actions, JavaScript actions in job containers,
  Docker actions, and nested composite actions.
- Bound storage, upload size, request rate, reservation lifetime, retention,
  and post-action execution time.
- Keep a stable internal semantic backend contract so GitHub cache service v2
  can be added as a second frontend later.

## Non-goals

- Do not proxy GitHub's cache service or use GitHub runtime credentials.
- Do not implement GitHub cache service v2/Twirp in this slice.
- Do not set `ACTIONS_CACHE_SERVICE_V2` or `ACTIONS_RESULTS_URL`.
- Do not implement `actions/upload-artifact`, `actions/download-artifact`, or
  the GitHub Actions results service. Artifact v4 uses a different protocol and
  lifecycle.
- Do not special-case `actions/cache`, setup actions, or `mise-action` in the
  executor.
- Do not inspect, extract, normalize, or execute cache archives server-side.
  Cache bytes are opaque.
- Do not make a cache hit authoritative. Existing action-source and managed
  runtime digest checks still run on hits.
- Do not expose a general-purpose Buildkite cache credential to action code.
- Do not claim compatibility for Windows, macOS, GHES, or cache v2 in this
  Linux-first slice.
- Do not make the existing `.buildkite-gha/cache-volume`/`MiseDataDir` volume
  the production GHA cache backend merely because it persists managed Node
  installations. It has a different purpose and no established GHA cache
  metadata or concurrency contract.

## Compatibility target: cache v1 first

Current cache clients select the legacy v1 protocol when
`ACTIONS_CACHE_SERVICE_V2` is absent. `actions/cache@v4` retains this path for
GHES and other runners that expose only `ACTIONS_CACHE_URL`; v4 does not require
the v2 service merely because it uses Node 20.

For every action invocation, the runtime supplies:

```text
ACTIONS_CACHE_URL=http://<runtime-owned-address>:<random-port>/
ACTIONS_RUNTIME_TOKEN=<cryptographically-random-job-token>
```

The URL must end in `/` because the public client appends
`_apis/artifactcache/...` directly. The runtime must leave these unset:

```text
ACTIONS_CACHE_SERVICE_V2
ACTIONS_RESULTS_URL
```

Do not set `ACTIONS_CACHE_SERVICE_V2=false`: the JavaScript client treats any
non-empty value as enabling v2.

The v1 adapter is a compatibility frontend. The internal entry model must not
embed v1-specific numeric IDs or URLs so a later v2/Twirp frontend can reuse
the same backend and policy.

## Architecture and trust boundaries

These are production-backend boundaries. The experimental directory backend
intentionally falls outside boundaries 2–5: it derives compatibility namespace
from the generated job environment and plan ref, and is safe only on volumes
shared by mutually trusted workflows. It is not an authorization boundary.

```diagram
┌────────────────────────────────────────────────────────────────────┐
│ buildkite-gha run-job                                              │
│                                                                    │
│  ┌──────────────────┐  cache URL + random token  ┌──────────────┐  │
│  │ action process   │───────────────────────────▶│ cache v1     │  │
│  │ host/container   │◀───────────────────────────│ adapter      │  │
│  └──────────────────┘    opaque archive bytes    └──────┬───────┘  │
│                                                        │ job-authenticated
│  Agent/job authority and transfer URLs stay private    │ API calls
└────────────────────────────────────────────────────────┼──────────┘
                                                         ▼
                                            ┌─────────────────────────┐
                                            │ Buildkite GHA cache     │
                                            │ visibility + metadata +│
                                            │ reservation + quota    │
                                            └────────────┬────────────┘
                                                         │ short-lived direct
                                                         │ PUT/GET instructions
                                                         ▼
                                            ┌─────────────────────────┐
                                            │ dedicated opaque object │
                                            │ prefix in blob storage  │
                                            └─────────────────────────┘
```

The boundaries are:

1. **The action-facing cache capability is local and random.** Generate at
   least 256 random bits, retain it only in memory, compare it in constant
   time, bind all reservation IDs to it, and invalidate it when `RunJob` exits.
   This value is `ACTIONS_RUNTIME_TOKEN`; it is not Buildkite job authority or
   a GitHub credential.
2. **The Agent API authenticates the job independently.** Every backend route
   is under `/v3/jobs/:job_id/github-actions-cache`; the server verifies the
   signed job identity matches that path and derives organization, cluster,
   pipeline, build, and job identity from authenticated records.
3. **The backend resolves and enforces visibility.** The server derives branch,
   tag, default branch, PR base, head repository, and fork status from trusted
   webhook-backed provider/build records. A client cannot select organization,
   cluster, pipeline, or ref scope IDs.
4. **All runtime-facing authority remains private.** Agent tokens, reservation
   tokens, backend sessions, signed storage URLs, and service credentials remain
   in the trusted parent process and are never copied into action environment,
   plans, generated pipeline YAML, result manifests, artifacts, or logs.
5. **Workflow fields are compatibility data, not storage authority.** The
   current plan `Event`, `github.*` context, `GITHUB_*`, `BUILDKITE_*` values
   visible to steps, request headers, cache keys, and request bodies must not
   select organization, cluster, pipeline, or ref namespace.
6. **Actions share job authority.** Restricting the token to action invocation
   environments prevents accidental exposure and matches the service contract;
   it is not a sandbox boundary against a hostile shell step in the same Unix
   account/job. That matches the project's existing job-level trust model.
7. **Downloads use a narrower opaque URL.** The public client does not attach
   `ACTIONS_RUNTIME_TOKEN` when fetching `archiveLocation`. Return a random,
   short-lived, job-local download URL which names no backend locator and is
   valid only for the adapter lifetime.

The adapter listens only for the job lifetime. Host actions use loopback.
Container actions use a runtime-owned Docker host alias and the same server
port. Container reachability broadens the listener from loopback to the
disposable job VM's Docker interfaces, so every control request still requires
the bearer token and every download URL must be unguessable and short-lived.

### Shared capability plane, separate protocols

Cache and token-requiring GitHub access may share a local runtime daemon and
common lifecycle, routing, redaction, and audit helpers. They must not reuse one
action-visible credential, upstream authority, or data protocol:

```text
GITHUB_TOKEN=<opaque GitHub API proxy capability>
ACTIONS_RUNTIME_TOKEN=<independent random local cache capability>
ACTIONS_CACHE_URL=<job-local cache v1 adapter>
```

The future GitHub API proxy capability has its own audience, routes, expiry,
repository permissions, and upstream GitHub App installation token. It cannot
authorize the cache adapter or backend Agent API, and the cache token cannot
call the GitHub proxy. Cache archives flow between the job-local adapter and
the dedicated cache object prefix using short-lived instructions returned by
the cache backend; they do not traverse the GitHub gateway.

The local component may eventually be one daemon hosting coordinated GitHub
and cache frontends or remain in-process in `run-job`. Either shape must keep
tokens and routes service-separated. It owns stable job-local capabilities,
container routing, and lifetime through post-actions, but never holds the
GitHub App private key. Cache metadata and publication remain owned by the
dedicated Buildkite backend rather than the GitHub proxy.

## GitHub cache v1 HTTP contract

All control endpoints require:

```http
Authorization: Bearer <ACTIONS_RUNTIME_TOKEN>
Accept: application/json;api-version=6.0-preview.1
```

Reject missing, malformed, or incorrect authorization before reading a large
body. Apply request-body and header limits before decoding.

### Restore lookup

```http
GET /_apis/artifactcache/cache?keys=<comma-separated-candidates>&version=<opaque-version>
```

The client sends `[primary key, restore key 1, restore key 2, ...]` as one
URL-encoded comma-separated query value. Support no more than ten candidates,
reject empty candidates and commas in decoded keys, and enforce the public
512-character key limit.

A miss returns:

```http
204 No Content
```

A hit returns a 2xx JSON body:

```json
{
  "cacheKey": "the-complete-stored-key",
  "cacheVersion": "the-exact-requested-version",
  "scope": "non-authoritative-display-scope",
  "creationTime": "2026-07-27T00:00:00Z",
  "archiveLocation": "http://<job-adapter>/downloads/<opaque-random-id>"
}
```

`cacheKey` must be the complete key of the selected entry, not the matching
restore prefix. `actions/cache` uses equality with the primary key to determine
`cache-hit` and whether its post phase should save a replacement.

The optional debug path should also be implemented because the toolkit calls it
after some misses when debug logging is enabled:

```http
GET /_apis/artifactcache/caches?key=<primary-key>
```

Return the bounded v1 `ArtifactCacheList` shape. This endpoint is diagnostic,
must obey the same scope rules, and must not reveal entries outside the caller's
read scopes.

### Reservation

```http
POST /_apis/artifactcache/caches
Content-Type: application/json

{
  "key": "primary-key",
  "version": "opaque-version",
  "cacheSize": 123456
}
```

On success, return a positive numeric job-local ID:

```json
{"cacheId": 123}
```

The numeric ID exists only because v1 requires it. Map it inside the adapter to
an opaque backend reservation ID and bind that mapping to the current job token
and write scope. Never expose a sequential backend primary key.

Reservation is atomic for:

```text
trusted namespace + write ref scope + key + version
```

Only one writer may hold or commit that identity. If an immutable committed
entry exists, or another unexpired writer owns the reservation, report
contention without replacing the entry. `409 Conflict` is the preferred local
status for contention; the client does not require that exact status and treats
an unusable reservation as a normal concurrent-save failure. Use `400` or
`413` for malformed/oversized requests, with bounded non-sensitive messages.

### Chunk upload

```http
PATCH /_apis/artifactcache/caches/{cacheId}
Content-Type: application/octet-stream
Content-Range: bytes <inclusive-start>-<inclusive-end>/*
```

The toolkit uploads chunks concurrently. The server must:

- validate the inclusive range and exact request-body length;
- accept chunks arriving and completing out of order;
- write by explicit offset rather than appending;
- accept an identical replay of an already stored range;
- reject inconsistent overlapping bytes;
- reject ranges outside the reservation's declared/allowed maximum;
- avoid retaining the entire archive in memory; and
- keep partial bytes invisible to restore lookup and download.

The client's default is four concurrent 32 MiB chunks, but environment options
can increase concurrency and chunk size. The implementation must not depend on
those defaults.

### Commit

```http
POST /_apis/artifactcache/caches/{cacheId}
Content-Type: application/json

{"size": 123456}
```

Before publishing the entry, verify that:

- `size` is non-negative and within configured limits;
- it agrees with the reservation's supplied size when one was supplied;
- uploaded ranges cover exactly `0..size-1` with no holes;
- replayed/overlapping ranges did not introduce inconsistent bytes; and
- the backend blob has the expected length.

Commit atomically changes the entry from reserved/uploading to committed.
Lookup can observe only the committed state. A retry of the same commit and
size must return success; a retry with a different size must fail. Once
committed, the entry is immutable.

### Download

`archiveLocation` is fetched without the runtime bearer token. Implement:

```http
GET  /downloads/{opaque-random-id}
HEAD /downloads/{opaque-random-id}
```

Return accurate `Content-Length`. Support standard single byte ranges and
`206 Partial Content` so the endpoint remains compatible with concurrent
download clients and future storage URL choices. Stream from the backend; do
not buffer whole archives in the adapter.

The opaque URL must:

- contain at least 128 random bits;
- be mapped only in memory to one committed backend entry;
- expire no later than the job adapter;
- be scoped to GET/HEAD for that entry; and
- never expose a backend object key, credential, tenant ID, or signed URL in
  logs.

### Errors, retries, and idempotency

The public client retries transport failures and 502/503/504 responses. A
response can be lost after the server has applied it, so these operations must
be replay-safe:

| Operation | Required replay behavior |
| --- | --- |
| Lookup/list | Naturally idempotent |
| Reserve | Return contention or the same still-valid job-local reservation; never create two writable entries |
| PATCH range | Accept identical bytes at the same range; reject inconsistent overlap |
| Commit | Repeating the same reservation and size succeeds |
| Download | GET/HEAD/range requests are side-effect free |

Use `429` for rate limits, `401` for invalid job tokens, `403` for a valid token
whose server-side mode denies the operation, `404` for unknown/expired local
IDs, `409` for contention/inconsistent replay, `413` for size limits, and
`503` for a temporarily unavailable backend. Error bodies and logs must remain
bounded and must not contain raw tokens, backend locators, or archive content.

## Entry identity and lookup semantics

The production identity is:

```text
organization UUID
+ cluster UUID
+ pipeline UUID
+ canonical ref scope
+ key
+ version
```

Repository names, pipeline slugs, branch names, and action-supplied keys may be
stored as bounded display metadata, but immutable IDs and canonical ref scopes
form the lookup boundary. Moving or renaming a pipeline must not accidentally
merge it with another pipeline's cache.

Treat `version` as an opaque exact-match value. The toolkit derives it from
path patterns, compression, platform, cross-OS mode, and a client format
version. The adapter does not reproduce or normalize that calculation.

For each permitted read scope, select in this order:

1. exact primary key;
2. newest primary-key prefix match;
3. each restore key in request order, selecting an exact match first and then
   the newest prefix match for that restore key; and
4. no match.

Search the current scope completely before moving to its permitted fallback
scope. Use committed creation time descending and immutable entry ID as a
stable final tie-breaker. Never let backend enumeration order decide a hit.

The selected full key is returned as `cacheKey`. Partial uploads, failed
commits, expired entries, disallowed scopes, and versions other than the exact
requested version are never candidates.

## Trusted scope policy

The backend session must receive immutable organization, cluster, pipeline,
build, and job identity from a Buildkite-authenticated source. Canonical
branch/PR/tag/default-branch facts must come from a trusted Buildkite build or
provider-event record. The current `buildkiteEventSource` and plan `Event` are
explicitly compatibility snapshots and are not sufficient as storage
authorization inputs.

The dedicated backend derives immutable organization, agent-cluster, and
pipeline UUIDs from the authenticated job. It accepts only GHA key, version,
candidate, reservation, and opaque entry identifiers from the client; unknown
JSON fields are rejected. Admission is limited initially to GitHub/GHES
pipelines, provider webhook builds, non-pipeline-trigger builds, an exact
authenticated cluster match, and the preview feature gate.

The server derives canonical branch/tag/PR/default/base/fork visibility from
persisted build and provider records. Missing or untrusted PR fork
classification disables the capability. This keeps provider facts out of the
client contract and makes the backend, not `buildkite-gha`, the enforcement
point. The capability endpoint returns `disabled`, `read-only`, or `read-write`
mode and limits for the authenticated job.

Initial policy:

| Build type | Ordered read scopes | Write scope |
| --- | --- | --- |
| Default branch | default branch | default branch |
| Normal branch | exact branch, then default branch | exact branch |
| Same-repository pull request | exact PR, then trusted base branch, then default branch | exact PR |
| Fork pull request | trusted base/default branch only | denied initially |
| Tag | exact tag, then default branch | exact tag |

Deduplicate scopes when the base branch is the default branch. A later policy
may allow writes to an isolated fork/PR namespace, but must not allow fork code
to populate base/default or same-repository PR caches.

Read/write mode is server-side authority:

```text
disabled | read-only | read-write
```

Do not merely send a mode hint to the client. A read-only token receives `403`
from reserve/PATCH/commit regardless of request data. A disabled service is not
advertised to actions. The runtime should fail before action execution if it
claims read-write cache availability but cannot establish a backend session;
rollout modes that intentionally provide no cache should leave the cache
environment unset rather than expose a dead URL.

## Internal backend contract

The v1 HTTP frontend owns job-local chunk assembly. The dedicated Buildkite GHA
cache backend owns durable cross-job metadata, reservation, policy, and blob
publication. This separation lets the adapter accept GHA's out-of-order/retried
PATCH requests without requiring the durable store to expose a chunk protocol.

The HTTP handler depends on a semantic interface, not on generated YAML, Agent
commands, object-store URLs, or Rails wire details. Its implemented contract is
equivalent to:

```go
type Backend interface {
    Lookup(context.Context, LookupRequest) (Entry, bool, error)
    List(context.Context, ListRequest) ([]Entry, error)
    Reserve(context.Context, ReserveRequest) (Reservation, error)
    Upload(context.Context, ReservationID, BlobSource) (Blob, error)
    Commit(context.Context, ReservationID, Blob) (Entry, error)
    Abort(context.Context, ReservationID) error
    Open(context.Context, EntryID, *ByteRange) (io.ReadCloser, BlobInfo, error)
}
```

Requests passed to this interface contain already-derived trusted namespace,
ordered read scopes or one write scope, validated key/version, and bounded
sizes. The action-facing HTTP request cannot populate namespace fields.

`Reserve` is a server-side conditional operation over verified scope, key, and
version; only its winner receives upload authority. The adapter maps that
opaque reservation to the positive numeric v1 `cacheId`, writes PATCH ranges
into one bounded sparse temporary file, validates complete coverage at v1
commit, and computes size plus SHA-256. `Upload` follows the backend's
short-lived, header-bound direct PUT instructions. `Commit` conditionally
publishes the exact reserved generation as immutable metadata after the server
HEAD-verifies size and checksum. `Open` performs policy-checked retrieval and
follows a short-lived range-capable GET instruction so the job-local HTTP server
can satisfy GET/HEAD and range requests.

Required metadata:

```text
entry ID
organization/cluster/pipeline namespace
canonical ref scope
full key
opaque version
creation time
committed size
blob locator/digest
expiration/retention metadata
state: reserved | committed | expired
```

Reservations also require owner/session identity, lease expiry, declared size,
received byte ranges, and enough integrity metadata to reject inconsistent
replays. Chunk ranges are job-local adapter state; the durable reservation needs
the expected entry identity, generation, lease, and eventual blob digest/size.
Backend object IDs and locators remain private.

The interface expresses required behavior, not necessarily one Agent or server
call per method. The production implementation maps it to the dedicated
job-authenticated API and direct transfer instructions. Do not add another
durable index inside `buildkite-gha`; policy, retention, generation, quota, and
contention have one production source of truth in `buildkite/buildkite`.

The in-memory backend remains for protocol, runtime, race, and fault-injection
tests. The bounded experimental directory backend is selectable only through an
explicit generated-job environment setting and is documented as a best-effort
integration bridge, not production semantics.

## Superseded Cache v2 assessment

> **Historical assessment only.** The decision to extend Cache v2 was reversed
> after comparing the required GHA semantics and platform schedules. The
> production plan is the independent backend below. This section preserves the
> evidence behind that decision and is not an implementation checklist.

The assessment inspected Buildkite Agent commit
`ba4e2b665c9501a3f81eb276aa1796ee20775f09`, current Cache registry models,
Notion policy decisions, and Linear delivery status on 2026-07-27.

### Confirmed reusable substrate

- One metadata registry belongs to a Buildkite cluster. Save scope values are
  stamped from verified Agent/job identity, and clients cannot select another
  organization, pipeline, or branch with legacy flags.
- Cache entries persist across builds. Hosted storage is Buildkite-managed
  Namespace storage; self-hosted agents can use an S3-compatible store with
  ambient credentials.
- Metadata moves through the Buildkite Cache API while archive bytes move
  Agent-to-store. Blobs are content-addressed by caller-computed digest.
- Restore applies registry-policy scopes in order, then exact structured key,
  then longest-to-shortest whole-key-part fallback, selecting newest
  `created_at` within a prefix on a best-effort eventually consistent scan.
- The low-level importable
  [`agent/v3/api` cache surface](https://github.com/buildkite/agent/blob/ba4e2b665c9501a3f81eb276aa1796ee20775f09/api/cache.go#L134-L263)
  accepts runtime target paths, structured keys, blob digest/size/compression,
  and exposes peek/create/commit/retrieve/expire. It does not transfer bytes;
  Agent orchestration and Namespace/S3 stores remain internal packages.
- `run-job` can retain Agent and Namespace authority in its stripped parent
  environment and expose only the local cache token to action processes.
  Current container jobs do not mount the Agent.

### Confirmed incompatibilities

| Required by this plan | Shipped Cache v2 behavior | Required change |
| --- | --- | --- |
| Store one already-created opaque archive | High-level CLI requires YAML, archives target paths into ZIP/Zstd, stages the complete wrapper, and extracts on restore | Public opaque blob put/get path with no wrapper or extraction |
| Identity independent of ephemeral local paths | Permanent address includes a hash of the configured target-path set | Opaque-entry address based on immutable organization/cluster/pipeline identity plus verified ref scope, key, and version only |
| Immutable organization/pipeline namespace | Server stamps organization owner and pipeline slugs; registry implies cluster | Stamp immutable organization/cluster/pipeline IDs for GHA entries so rename/reuse cannot merge authority |
| Ordered arbitrary GHA key candidates | Structured key parts with contiguous trailing whole-part fallback | One policy-checked lookup accepting primary key, ordered restore keys, exact opaque version, and flat string-prefix matching |
| Newest matching entry deterministically | Exact reads are strongly consistent; fallback scans are eventually consistent and bounded by 256 RCUs | Explicit consistency/newest contract suitable for GHA lookup |
| Atomic reservation and immutable first commit | Five-minute temporary upload record followed by unconditional metadata `PutItem`; concurrent later commit overwrites earlier metadata | Conditional reserve/commit with generation token and first-writer-wins immutability |
| Dynamic default/base and PR/fork visibility | Policy scopes only `pipeline`, `branch`, `build`; no default branch, PR/base/head repository, fork, provider event, or canonical ref-kind claims | Validate a signed visibility/scope grant from the shared capability gateway, or add an equivalent Cache v2-native resolver |
| Verified opaque bytes | Backend trusts caller digest; restore checksum verification is open | Verify digest before publication/use and generation-guard invalidation/TTL changes |
| Stable supported integration | Hidden experimental CLI plus generated YAML; public API is metadata-only and stores are internal | Narrow supported Agent package or machine-safe subprocess/API contract |

Do not use Cache v2 `peek` as an authorization-bearing restore operation: it is
metadata-only and bypasses registry restore policy. Use a policy-checked
retrieve/lookup operation. Do not encode a GHA string into one Cache v2 key part
and assume fallback works: current fallback searches descendants at whole-part
boundaries and cannot match an arbitrary prefix such as `npm-linux-` against
`npm-linux-abc`.

Current publication is explicitly not a reservation. Two jobs can both miss,
upload, and commit; the later unconditional commit replaces metadata. The stock
Agent's pre-save peek narrows but does not close that race. Open hardening work
includes checksum verification
([A-1494](https://linear.app/buildkite/issue/A-1494/verify-cache-checksum-on-restore)),
content upload deduplication
([A-1495](https://linear.app/buildkite/issue/A-1495/skip-re-uploading-a-cache-when-the-content-already-exists)),
and generation-safe invalidation
([A-1496](https://linear.app/buildkite/issue/A-1496/prevent-cache-invalidation-from-deleting-a-freshly-re-uploaded-entry)).

### Required narrow Cache v2 extension

Deliver these as one reviewed Cache v2/Agent contract rather than a second
`buildkite-gha` service:

1. **Opaque entry transfer and identity.** Accept runtime key/version and one
   input/output archive path or stream without YAML, re-archiving, or
   extraction. Address the entry by immutable organization/cluster/pipeline
   identity, verified ref scope, key, and version—not the ephemeral archive
   path. Add an explicit opaque content/compression type. Hash on upload and
   verify on download. Keep Namespace/S3 authority inside the Agent-side
   implementation.
2. **GHA-compatible policy lookup.** In one policy-checked server operation,
   accept an exact version plus ordered primary/restore candidate strings;
   evaluate current/fallback ref scopes in server policy order; perform
   exact-then-flat-string-prefix matching for each ordered candidate; and return
   the deterministic newest committed match and its complete key. Do not
   approximate this with sequential `peek` calls.
3. **Conditional reservation and commit.** Reserve verified
   scope+key+version atomically, expose an expiring opaque generation only to
   the winner, keep uploads invisible, conditionally commit that generation
   once, make committed entries immutable, and make same-generation retries
   idempotent. Generation-guard expiration, invalidation, and TTL refresh.
4. **Verified ref visibility.** Accept and validate a cache-audience grant from
   the shared capability gateway containing immutable tenant/job/plan binding,
   canonical ref kind, ordered read scopes, one write scope, mode, and expiry.
   The gateway derives default branch, PR/base/head repository/fork status, and
   tag identity from authenticated Buildkite/provider records. An equivalent
   Cache v2-native resolver is acceptable, but Cache v2 remains the enforcement
   point and never trusts client-selected tenant or scope IDs.
5. **Integrity and preview hardening.** Land checksum verification and
   generation-safe invalidation, define prefix consistency/newest behavior,
   decide whether fallback hits refresh TTL, and publish archive/entry/byte
   quotas plus retention semantics.
6. **Supported Agent boundary.** Expose the operations through a public Agent
   package or a versioned machine-safe command/API intended for callers such as
   `buildkite-gha`. Avoid credentials, tokens, or unbounded request JSON on
   command lines. Document Hosted Namespace and self-hosted S3 support.

The feature can remain generic where semantics are genuinely shared, but the
lookup and visibility contract must represent GHA behavior directly rather
than forcing it through Cache v2's current structured key encoding.

### Agent and rollout prerequisites

Hosted execution needs the Agent binary, job-scoped Agent access token and
endpoint, cluster-default registry, injected `nsc://` cache store URL, and
ambient `nsc` authority. These remain in `run-job`; action code receives none of
them. Source history places Cache v2 wire support at Agent v3.128.0, Hosted
Namespace storage at v3.130.0, and complete assessed behavior at v3.134.0.
Hosted deployment of v3.133.0 is confirmed by
[A-1555](https://linear.app/buildkite/issue/A-1555/deploy-buildkite-agent-v31330-to-hosted-agent);
confirm v3.134+ coverage or the eventual new-feature version before enabling
the profile.

Production trusted scoping additionally needs Job OIDC acquisition and refresh
for the exact capability-gateway audience, gateway provider-provenance support,
and Cache v2 trust configuration for the cache-grant issuer and keys. The
adapter must fail closed without that path. This requirement does not put Job
OIDC or the signed grant into action environment, and it does not require the
gateway's GitHub REST/GraphQL proxy to ship first.

Cache archive format v2 remains in review under
[A-1584](https://linear.app/buildkite/issue/A-1584/cache-archive-format-v2-manifest-based-layout),
and Cache v2 end-to-end coverage remains tracked by
[A-1545](https://linear.app/buildkite/issue/A-1545/bring-up-e2e-coverage-for-cache-v2-restoresave-flows).
The opaque mode should not depend on Agent-created archive format, but preview
still requires Cache v2 feature enablement, Hosted Agent rollout, storage/TMPDIR
tests, quotas/retention, and operational ownership.

## Independent Buildkite backend contract

The selected backend is a standalone `GitHubActionsCache` domain in the
Pipelines database shard. It has no Cache v2 model or protocol dependency.
Buildkite owns durable metadata, atomic coordination, trusted ref visibility,
quota/retention policy, and short-lived direct blob-transfer instructions.
Blob bytes do not pass through Rails.

The current platform implementation exposes these job-authenticated routes
under `/v3/jobs/:job_id/github-actions-cache`:

| Operation | Request | Response purpose |
| --- | --- | --- |
| capability | `GET /` | `enabled`, mode, limits, and disabled reason |
| lookup | candidates and exact version | policy-filtered hit metadata or miss |
| reserve | key, version, optional declared size | expiring reservation ID and token |
| prepare upload | reservation credentials, size, SHA-256 | short-lived PUT plus mandatory signed headers |
| commit | reservation credentials | HEAD-verified immutable entry |
| abort | reservation credentials | idempotent terminal status |
| retrieve | opaque entry ID | reauthorized entry plus range-capable GET |

The namespace is the authenticated organization UUID, agent-cluster UUID, and
pipeline UUID. Lookup is server-side scope order, then each ordered candidate,
then exact before newest prefix, with exact opaque version filtering. A unique
canonical address plus row/advisory locking provides one active reservation and
immutable first-writer publication. Reservation and entry IDs, tokens, and blob
keys are unguessable and generation-bound.

The current implementation uses the existing S3-compatible artifact client
only as a generic object-store client under a dedicated opaque
`github-actions-cache/blobs/` prefix. It creates no Artifact record. PUT
instructions bind content length and SHA-256 headers; commit verifies both with
HEAD before publication. Retrieve accepts only an opaque entry ID, rechecks
policy, and never exposes a durable blob key.

This platform work is open in [`buildkite/buildkite` PR
#31646](https://github.com/buildkite/buildkite/pull/31646) but remains unmerged,
undeployed, preview-gated, and disabled by default. Before a Hosted preview:

1. confirm the production bucket/client, prefix-scoped IAM, encryption,
   monitoring, and object-lifecycle backstop;
2. ship the bounded garbage-collector worker and its schedule in the required
   separate deployment changes;
3. confirm retention, byte/entry, archive-size, and reservation policy values;
4. verify webhook-backed PR repository/base/fork fields for all admitted
   GitHub and GHES paths;
5. switch generated jobs to the implemented
   `BUILDKITE_GHA_CACHE_BACKEND=agent` backend and validate absent, disabled,
   and read-only capability modes without exposing job authority or transfer
   instructions to workflow children; and
6. run direct, transitive, container, race, and cross-build canaries before
   enabling selected Hosted organizations.

## Runtime integration

### Lifecycle

For every admitted job containing one or more `uses` steps, including local
actions:

1. `run-job` queries the job-authenticated capability endpoint and establishes
   the dedicated backend client before executing workflow code. Disabled or
   intentionally cache-less operation leaves the action cache environment
   unset; claimed read-write availability fails closed if no session can be
   established.
2. `Runner.RunJob` creates the random bearer token and registers it with both
   the in-process command processor and Buildkite Agent redaction before any
   action can print it.
3. Start the HTTP server on a random OS-assigned port and wait until it is ready
   before action preparation/pre phases.
4. Keep one service/session for the complete job so pre, main, nested composite
   children, concurrent actions, and post phases share reservation state.
5. Refresh only short-lived backend transfer instructions as required without
   changing the stable action-facing cache token. New backend operations fail
   closed if authenticated Agent/job authority is no longer valid.
6. Drain all registered action post phases before shutting down the adapter.
7. Stop accepting new reservations, finish or cancel in-flight requests under
   a bounded deadline, abort incomplete reservations, revoke download IDs, and
   close the backend session.
8. Perform ordinary short resource cleanup after the cache/post-action phase.

If a job has no Actions, do not start the adapter. Do not infer use from only
remote action locks: a local action can contain a cache client.

### Environment injection

Add cache variables at the action invocation boundary in
`internal/runtime/job.go`, after workflow/job/step environment evaluation so a
workflow cannot override the runtime-owned values:

- JavaScript pre/main/post actions receive them;
- JavaScript children of composite actions receive them;
- Docker action processes receive a container-reachable URL;
- JavaScript actions running in a job container receive a container-reachable
  URL; and
- shell `run` steps, including shell children in composite actions, do not
  receive them.

The runtime-owned values win over workflow, job, step, action metadata, and
`GITHUB_ENV` attempts to set the same names. Add validation tests for this
precedence. Explicitly remove workflow-supplied `ACTIONS_RESULTS_URL` and
`ACTIONS_CACHE_SERVICE_V2` from action invocation environments; allowing a
workflow to set either would switch the toolkit away from the supported v1
frontend.

Do not copy the cache environment into `jobResult.Env`. That map is propagated
between steps and serialized into bounded result handling. Keep cache
environment in a separate runtime-owned action overlay.

### Host and container networking

Host JavaScript actions use:

```text
http://127.0.0.1:<port>/
```

Loopback does not reach the host from a job container or Docker action. For
container action invocations:

- bind the adapter to the disposable job VM interfaces on the same random
  port;
- add one runtime-owned Docker host alias, for example
  `buildkite-gha.internal:host-gateway`, to persistent job containers and
  one-shot Docker action containers;
- use `http://buildkite-gha.internal:<port>/` in their action-only environment;
- allow only the runtime-owned hostnames when constructing absolute download
  URLs; do not trust an arbitrary HTTP `Host` header; and
- require the bearer token on every control request even though the network is
  job-local.

Probe `host-gateway` support on the supported Hosted Docker version. A host job
with a Docker action but no job container/services must still receive the alias
and reach the adapter. Preserve the existing private Docker configuration,
fixed mount, local builder, and owned-resource cleanup boundaries.

### Post-action timeout

The runtime now separates the post-action and resource-cleanup budgets:

- retain the short cleanup timeout for Docker/process/resource cleanup;
- use a separately configurable post-action drain timeout with a ten-minute
  default;
- keep one bounded LIFO post phase, rather than granting each action the full
  budget independently;
- keep the cache server and backend session alive through that phase;
- preserve `post-if` evaluation and job timeout/cancellation semantics; and
- allow explicit build/agent shutdown to cancel a stuck post upload without
  skipping final resource cleanup.

Tests prove a post action can exceed the short cleanup budget and complete
within the independent post budget, while explicit cancellation still observes
the short cleanup grace. Confirm the ten-minute production default after
measuring representative Hosted uploads.

## Plan, admission, and reporting changes

No job-plan schema change is required for the first slice. Cache is a baseline
runtime facility for Actions jobs, not a workflow-selected provider credential.
Backend reservation credentials, Agent/job authority, direct transfer URLs,
and service credentials must not appear in immutable plans.

The compiler and runtime still need auditable behavior:

- generated jobs continue to carry whether they use Actions;
- the fixed `hosted-tokenless` profile reports cache service availability only
  when the production backend and scope policy are enabled on that queue;
- `validateUnprivilegedBundle` currently admits `actions/cache` onto the
  explicit experimental Hosted backend; production support remains gated on
  the durable backend client and live evidence;
- artifact actions remain rejected with a precise artifact-service diagnostic;
- unknown transitive cache users are not rejected or specially classified;
- runtime startup says whether cache is disabled, read-only, or read-write
  without logging namespace IDs, keys, token, or backend locators; and
- the existing unsupported-cache smoke fixture becomes a runtime compatibility
  fixture after the service is enabled.

If future installations need per-job service declarations, add a versioned,
non-secret plan field such as:

```yaml
services:
  cache:
    protocol: gha-v1
    mode: read-write
```

Do not add that schema now without a consumer that needs it. Such a declaration
would be an auditable request only; the runtime/backend would still derive and
enforce actual authority.

## Operational controls

The production backend must enforce, server-side:

- maximum archive size and per-request body size;
- maximum keys per lookup and key/version lengths;
- per-pipeline and per-organization retained-byte/entry quotas;
- per-job concurrent reservations and in-flight byte limits;
- upload/download request and byte-rate limits;
- short reservation leases and abandoned-upload cleanup;
- retention and deterministic LRU/expiration policy;
- immutable first-successful-commit-wins behavior;
- fork write denial and ref-scope read policy;
- job-authenticated API validation, reservation expiry, and wrong-job/path
  rejection;
- bounded metadata/list responses; and
- backend/session revocation when the Buildkite job finishes.

Limits must be configurable by trusted installation policy, not action inputs.
Publish the selected defaults before enabling the hosted profile. Reject over
limit work before receiving large bodies where possible.

Emit low-cardinality metrics and structured events for:

- adapter/backend session start and failure;
- lookup hit/miss/error and match class (exact/primary-prefix/restore-prefix);
- reserve success/contention/denial;
- uploaded/downloaded bytes and duration;
- commit success/validation failure/backend failure;
- abandoned reservations and eviction; and
- post-action timeout/cancellation.

Dimensions may include backend kind, queue, mode, scope class, result, and
protocol version. Do not put tokens, full organization/pipeline IDs, raw cache
keys, versions, refs, URLs, or backend locators in logs or metric labels. A
stable one-way namespace hash may be used only where operational correlation
requires it.

## Implementation slices

### C0 — Protocol fixture and differential harness

Status: **Implemented.**

Deliver:

- an `internal/cache` semantic model and deterministic in-memory backend;
- an `httptest` v1 handler implementing lookup/list/reserve/PATCH/commit and
  download;
- request/response fixtures captured from pinned current toolkit clients;
- a small harness that runs the real v1 client against the adapter and records
  method, path, bounded headers, body shape, and result;
- fault injection for retries, contention, out-of-order chunks, dropped commit
  responses, missing ranges, and backend failures; and
- package-level race tests for parallel uploads and lookups.

Exit criteria:

- `actions/cache@v3` and v4 client fixtures produce the documented v1 traffic;
- leaving v2 variables unset is asserted; and
- no production backend or admission claim is made.

### C1 — Job-local adapter and host JavaScript actions

Status: **Implemented.**

Deliver:

- cache service lifecycle in `Runner.RunJob`;
- random token generation, constant-time authentication, redaction, local
  numeric reservation mapping, and opaque download URLs;
- action-only environment injection for JavaScript pre/main/post and nested
  composite action invocations;
- streaming temporary-disk chunk assembly for integration tests;
- clean adapter shutdown and reservation abort; and
- a two-job test that saves and then restores a direct `actions/cache` archive
  through a shared test backend.

Exit criteria:

- host JavaScript actions save during post and restore exact bytes;
- a shell step cannot observe cache variables through its supplied environment
  or job result;
- token and download URL masking tests pass; and
- v2 selectors remain absent so pinned v3/v4 clients use the v1 path.

### C2 — Independent backend and authenticated client

Status: **The platform side is review-ready and green at commit `bf9475e3e1d`
in `buildkite/buildkite` PR #31646, and the exact-contract `buildkite-gha`
client is implemented. Human review, merge, deployment, migration, feature
activation, and production selection remain open.**

Deliver the standalone Rails domain and its `buildkite-gha` adapter:

- one job-authenticated capability endpoint with disabled/read-only/read-write
  mode and server limits;
- policy-checked ordered lookup with exact version and exact-before-prefix
  semantics;
- conditional expiring reservation, generation-bound immutable commit,
  idempotent abort, and stale-reservation cleanup;
- direct opaque PUT/GET instructions without wrapping, recompression,
  extraction, or bytes traversing Rails;
- signed content-length/checksum upload headers and commit-time HEAD
  verification;
- authenticated backend-session setup in `run-job` without forwarding Agent
  authority, reservation tokens, or transfer URLs to workflow children;
- the semantic `Backend` implementation over that API; and
- bounded error mapping for malformed input, authorization, races, integrity,
  quota, and object-store failure.

Exit criteria:

- the platform branch is reviewed, merged, migrated, and feature-gated;
- two separate Buildkite jobs/builds restore a committed entry;
- concurrent jobs cannot both reserve or commit one verified
  scope+key+version identity;
- an opaque GHA archive reaches and returns from the store without byte changes
  or another archive wrapper;
- ordered flat-string prefix lookup returns the deterministic newest committed
  match through the policy-checked operation;
- backend outage and retry behavior is bounded and observable; and
- the experimental directory backend is not selected once the production
  capability is enabled.

### C3 — Trusted scoping and abuse controls

Status: **Implemented and review-green at commit `bf9475e3e1d` in
`buildkite/buildkite` PR #31646; human review, merge, operational approval, and
production-field validation remain open.**

Deliver:

- job-authenticated derivation of immutable Buildkite
  organization/cluster/pipeline, build, and job identity;
- authoritative provider provenance and normal branch, default branch,
  PR/base/head repository/fork, and tag scope derivation;
- the ordered read/write policy table in this plan;
- disabled/read-only/read-write enforcement;
- size, rate, entry, byte, and reservation limits;
- stable newest-prefix ordering and expiry; and
- security tests proving client-selected fields cannot cross namespaces.

Exit criteria:

- default, branch, same-repository PR, fork PR, and tag tests observe only their
  permitted scopes;
- pipeline/organization rename and slug reuse cannot inherit another immutable
  pipeline namespace;
- fork writes are denied;
- expired, replayed, stale-generation, wrong-job, and broadened-scope
  operations are rejected;
- Buildkite job authority is absent from action environment and a GitHub proxy
  capability cannot authenticate either the local adapter or backend Agent API;
- raw compatibility event/plan/env changes cannot alter storage authority; and
- cache entries intentionally cross builds only inside the same permitted
  pipeline/ref namespace.

### C4 — Containers and post-action lifecycle

Status: **Implemented.**

Deliver:

- the runtime-owned Docker host alias and container-specific cache URL;
- reachability from persistent job containers and one-shot Docker actions,
  including host jobs with no service containers;
- separate multi-minute post-action and short resource-cleanup budgets;
- cancellation and service-shutdown ordering; and
- no cache environment on composite shell children.

Exit criteria:

- host action, job-container action, Docker action, and nested composite
  fixtures save and restore;
- a post save can run longer than ten seconds without making cleanup unbounded;
  and
- cancellation leaves no adapter listener, reservation, container, network, or
  temporary archive behind.

### C5 — Out-of-box compatibility and rollout

Status: **In progress.** The pinned v4 restart-persistence canary passes
locally. Direct v4 completes Hosted miss/save; the unchanged transitive
`lox/notion-cli` workflow completes Hosted miss/save plus its `mise run test`
and `mise run lint`. Three sequential direct canary jobs each received an empty
named volume, so cross-build Hosted restore remains open pending the production
backend or a demonstrated later volume parent. The runtime now admits and
executes exact managed Node 16.20.2 for the final `actions/cache@v3` release;
all v3 releases declare `runs.using: node16`, so v3 cannot be covered honestly
by the existing Node 20/24 runtime set. At exact implementation commit
`9f76ae18aa492511fa436cce74f3c7c3f6cfb6fe`, the generated canary jobs in
[build 217](https://buildkite.com/buildkite/buildkite-gha/builds/217),
[build 216](https://buildkite.com/buildkite/buildkite-gha/builds/216), and
[build 218](https://buildkite.com/buildkite/buildkite-gha/builds/218) passed
real pinned cache-v3, setup-node v6.5.0, and setup-go v6.5.0 miss/workload/save
paths respectively. Their logs confirmed successful v1 saves of 309-,
26,729,250-, and 84,673,891-byte archives. These generated-job results do not
claim that unrelated parent-build checks passed or that a later build can
restore the directory-backed entries.

Deliver:

- preservation of the landed cache admission while artifact actions remain
  rejected;
- conversion of the unsupported-cache smoke fixture to runtime-pass evidence;
- real pinned `actions/cache@v3` and v4 canaries;
- setup-node and setup-go fixtures with caching enabled;
- an unchanged `jdx/mise-action` cache path;
- the current `lox/notion-cli` CI workflow as an unchanged canary;
- compatibility report and user documentation updates; and
- hosted feature rollout with rollback to disabled/read-only mode.

Exit criteria:

- cache-dependent workflows run without Buildkite-specific YAML edits;
- all security, backend, container, and post-action gates are green on an exact
  release commit; and
- disabling cache removes the action environment cleanly without exposing a
  dead or partially authorized service.

### Later — GitHub cache service v2 and artifacts

Only after v1 is reliable:

- add a cache v2/Twirp frontend over the same entry/backend model;
- add the required Azure Blob-compatible upload/download behavior if current
  v2 clients require it;
- set `ACTIONS_CACHE_SERVICE_V2`/`ACTIONS_RESULTS_URL` only for jobs served by
  that complete frontend; and
- design artifact v4/results compatibility as a separate service with its own
  identity, retention, cross-job, and UI semantics.

## Test matrix

### Protocol and storage

- v1 URL trailing-slash behavior and authorization headers;
- miss, exact hit, primary-prefix hit, and ordered restore exact/prefix hit;
- an exact restore-key hit preferred over a newer prefix match for that same
  restore key;
- exact opaque version mismatch;
- deterministic newest-entry and tie-break ordering;
- debug list endpoint scope filtering and bounds;
- atomic reservation and committed-entry contention;
- concurrent out-of-order chunk upload;
- identical PATCH replay and inconsistent overlap rejection;
- missing range, wrong body length, wrong commit size, and oversize rejection;
- response loss followed by reserve/PATCH/commit retry;
- partial upload invisibility and lease expiry;
- immutable first-commit-wins behavior;
- GET, HEAD, byte ranges, accurate content length, and interrupted download;
- zero-byte archive if the client permits it; and
- backend failures before reserve, during upload, commit, lookup, and download.

### Runtime and compatibility

- real pinned `actions/cache@v3` and `actions/cache@v4` save/restore across two
  runtime jobs;
- v4 selecting v1 because v2 variables are absent;
- cache save in a JavaScript post phase;
- pre/main/post and nested composite action cache environment;
- no cache environment in ordinary or composite `run` steps;
- workflow/job/step/`GITHUB_ENV` attempts cannot override runtime values;
- two parallel cache-using actions share the service safely;
- setup-node npm caching and setup-go module/build caching;
- `jdx/mise-action` with its default cache behavior; and
- the current `lox/notion-cli` CI workflow unchanged.

### Scope and security

- organization, cluster, and pipeline isolation;
- Buildkite job authority never present in an action environment, result, plan,
  artifact, generated pipeline, or log;
- GitHub proxy and local cache tokens rejected by one another's routes;
- expired, wrong-job, stale-generation, broadened-scope, and replayed
  reservation operations rejected as appropriate;
- default branch read/write;
- branch own-first/default-fallback reads and own-only writes;
- same-repository PR own/base/default reads and PR-only writes;
- fork PR base/default reads and denied writes;
- tag exact/default reads and tag-only writes;
- forged plan event, `GITHUB_*`, `BUILDKITE_*`, query, header, and body scope
  inputs cannot change authority;
- wrong/missing/expired token and cross-job reservation IDs fail;
- token, opaque download ID, backend locator, and error bodies are masked or
  omitted from all output/result paths;
- body, header, list, range, concurrency, rate, and storage limits; and
- cache contents are never interpreted or made authoritative.

### Execution environments

- host JavaScript action;
- JavaScript action in a persistent job container;
- Docker action in a host job without services;
- Docker action sharing the job runtime network;
- nested remote/local composite action;
- cancellation during restore, chunk upload, commit, and post save; and
- agent/process loss followed by abandoned reservation cleanup.

Run protocol/unit/race tests locally. Run real actions, the dedicated Buildkite
backend, container networking, cross-build persistence, and unchanged external
workflow canaries in an explicit networked/live lane against one exact commit.
A successful static compile or admission result is not runtime cache evidence.

## Rollout

1. Land C0/C1/C4 and the experimental directory backend without calling it a
   production authorization or concurrency boundary.
2. Use the named Hosted cache volume as an integration bridge. Direct v4
   miss/save is proven; builds 205–207 did not receive a prior committed volume,
   so do not promote the smoke classification or block the production backend
   on a best-effort volume hit. Build 212 proves the current transitive
   `jdx/mise-action@v2` miss/save path and unchanged external workflow, but not
   cross-build restore.
3. Review and merge the independent Rails domain, migration, feature gate, and
   job-authenticated Agent API while the preview remains disabled.
4. Confirm object-store IAM/lifecycle, ship GC worker and schedule, validate
   webhook provider fields, and approve quotas/retention.
5. Switch generated production jobs to the implemented
   `BUILDKITE_GHA_CACHE_BACKEND=agent` backend and exercise the capability in
   absent, disabled, and read-only modes without exposing action cache variables
   when unavailable.
6. Enable read-write for dedicated test organizations and run direct,
   transitive, container, PR/fork, and external workflow canaries on exact
   commits.
7. Retire the experimental directory backend from generated production jobs
   before broader Hosted support. Treat self-hosted support as separate until
   its object-store and networking contract is explicit.

Rollback is server-side mode `disabled` or `read-only`, plus restoring the
hosted-profile admission diagnostic if the service is no longer guaranteed.
Rollback must not require a workflow edit or expose stale credentials. Existing
committed cache entries may expire normally and are never migration authority.

## Definition of done

- Direct and transitive public `@actions/cache` v1 clients work without GitHub
  service credentials or workflow-specific executor code.
- `actions/cache@v3` and v4 restore/save exact bytes across Buildkite jobs and
  builds; v4 is explicitly proven on the v1 path.
- The current `lox/notion-cli` CI workflow works unchanged with the default
  `jdx/mise-action@v2` cache behavior.
- Setup-node and setup-go caching work without disabling their cache inputs.
- The dedicated Buildkite GHA backend contract, availability, limits,
  retention, atomicity, object lifecycle, GC, and support ownership are
  documented and tested.
- Namespace and ref policy is based on trusted Buildkite identity, with fork
  writes denied and no unexpected cross-organization/cluster/pipeline/ref
  access.
- Concurrent reservations, out-of-order/retried chunks, commit replay,
  abandoned uploads, expiry, and quota failures are deterministic and bounded.
- Cache tokens and backend authority never enter plans, pipeline artifacts,
  result manifests, ordinary run-step environment, or logs.
- Buildkite job authority remains in the trusted runtime, and cache and GitHub
  proxy capabilities are route-separated and non-interchangeable.
- Host, job-container, Docker, and nested composite actions pass live cache
  canaries.
- Cache post saves have a bounded multi-minute budget, while process/container
  cleanup retains its independent short bound.
- The hosted profile no longer rejects cache use, continues to reject artifact
  service use, and reports service availability honestly.
- Cache misses, evictions, contention, and backend outages remain observable
  cache outcomes rather than integrity or authorization bypasses.

## Open questions

Backend selection, lookup semantics, first-writer publication, identity, and
the Agent API shape are settled. These operational/product questions remain:

1. Is the existing Hosted artifact S3 client and bucket the approved production
   boundary for the dedicated prefix, and what exact prefix-scoped IAM,
   encryption, capacity monitoring, and object-lifecycle policy will apply?
2. What worker cadence and batch limits should garbage collection use, and what
   storage lifecycle backstop covers abandoned objects if the worker is down?
3. Are seven-day retention, the 5 GiB inclusive archive limit, the 10 GiB
   retained-byte namespace quota, 1,000 retained records per namespace, and the
   current reservation limits the approved preview values?
4. Are webhook-derived PR base branch, head repository, and fork fields present
   and authoritative for every GitHub and GHES event path admitted by the
   feature gate?
5. Which supported Hosted Agent/API deployment first guarantees all seven
   routes, and how should `buildkite-gha` distinguish absent, disabled,
   read-only, and temporarily unavailable capability responses?
6. Which GHA archive compression variants and practical maximum sizes must the
   preview accept, and what free-space preflight prevents sparse staging plus
   client archive creation from exhausting `$TMPDIR`?
7. Should the experimental named-volume backend be removed entirely when the
   remote capability is available, or retained behind a development-only
   switch that generated Hosted jobs never set?
8. Which team owns dashboards, abuse response, retention changes, GC, support,
   and rollout/rollback across the Rails backend, object store, Hosted, and
   `buildkite-gha`?

None blocks the local protocol/runtime work. The completed best-effort Hosted
canary did not produce cross-build persistence, so production enablement now
depends on the independent durable backend unless the Hosted volume behavior is
separately resolved. The remaining questions must not be papered over by
embedding customer storage credentials, Agent authority, or a second production
durable index in `buildkite-gha`.
