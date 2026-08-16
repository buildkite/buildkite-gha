# GitHub Actions OIDC compatibility

## Problem

Workflows that authenticate to cloud providers declare `permissions:
id-token: write` and call actions such as `aws-actions/configure-aws-credentials`,
`google-github-actions/auth`, `azure/login`, and `hashicorp/vault-action`.
`internal/workflow/parse.go` rejects the `id-token` permission, so these
workflows fail compilation and cannot migrate without edits.

Those actions never contact GitHub directly. They call
`actions/core.getIDToken(audience)`, which:

- reads `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`
- appends `&audience=<encodeURIComponent(audience)>` to the URL, so the
  configured URL must already contain a query string
- sends a bearer-authenticated GET with up to 10 retries
- requires a JSON response `{"value": "<jwt>"}`

That endpoint contract is the entire runner-side OIDC surface. Emulating it
lets these workflows run unchanged.

## Outcome

Jobs that declare `permissions: id-token: write` mint Buildkite OIDC tokens
through the GitHub Actions endpoint contract. The workflow file does not
change. The cloud-side trust configuration changes once: an identity provider
for `https://agent.buildkite.com` with subject and audience conditions in
Buildkite's claim format, instead of GitHub's.

No one can sign tokens as `token.actions.githubusercontent.com`, so
GitHub-issued claims are out of scope by construction. The token is the same
Buildkite OIDC token `buildkite-agent oidc request-token` produces, with the
default compound subject
`organization:<org>:pipeline:<pipeline>:ref:<ref>:commit:<sha>:step:<key>`.
`buildkite-gha` assigns a step key per imported workflow job, so trust
policies can pin to individual workflow jobs.

## Design

### Request flow

```
action step (actions/core getIDToken)
  │ GET http://127.0.0.1:<port>/idtoken?api-version=2&audience=<aud>
  │ Authorization: Bearer <per-invocation request token>
  ▼
loopback ID-token service (runtime, per job with id-token: write)
  │ POST <endpoint>/jobs/<job-id>/oidc/tokens {"audience": "<aud>", ...}
  │ Authorization: Token <job token>
  ▼
Buildkite Agent API (same endpoint the agent CLI wraps)
```

### Compiler and plan

`adaptPermissions` accepts `id-token: read|write|none` instead of failing.
The job plan records the grant. The `id-token` scope is not a GitHub token
permission: it must never reach `WorkflowToken` or
`ValidateGitHubWorkflowAccessTokenPermissions`, and it adds no `GITHUB_TOKEN`
authority.

Matching GitHub exactly: the endpoint environment variables exist only in
jobs that explicitly declare `id-token: write`. In every other job, actions
fail with the same missing-variable error they produce on GitHub.

### Minting client

A new `AgentOIDCTokens` provider in `internal/runtime` follows
`AgentGitHubTokens` and `AgentCacheCredentials`: job-bound URL construction
and validation, `Authorization: Token <job token>`, bounded response size,
`DisallowUnknownFields`, and JWT-shape validation of the returned `token`
field. Request lifetime is omitted, selecting the API default of five
minutes, which matches GitHub's exchange window.

### Loopback ID-token service

The runtime starts one loopback HTTP listener per job, only when the plan
grants `id-token: write`. The handler:

1. requires a per-invocation random bearer token compared in constant time
1. reads `audience` from the query string; empty audience selects Buildkite's
   default `https://buildkite.com/<org>`
1. mints through `AgentOIDCTokens`
1. registers the token with the Buildkite Agent redactor and the local
   workflow-command redactor before responding `{"value": "<jwt>"}`

Environment injection follows the `cacheActionEnvironment` per-invocation
scoping pattern: `ACTIONS_ID_TOKEN_REQUEST_URL` (with a query string already
present, because the client appends with `&`) and
`ACTIONS_ID_TOKEN_REQUEST_TOKEN`.

### Agent OIDC options

The agent CLI surface maps onto this design as configuration ownership, not
action-time parameters:

| Agent option | buildkite-gha |
| --- | --- |
| `--audience` | Query parameter; the only knob the GitHub contract gives actions |
| `--lifetime` | Fixed API default (five minutes) |
| `--claim`, `--aws-session-tag`, `--subject-claim` | Plugin `oidc` configuration applied to every mint (later slice) |
| `--format gcp` | Not needed; `google-github-actions/auth` consumes the raw JWT |
| `--skip-redaction` | Never; both redactors always apply |

GitHub owns `sub` customization through a repository-level REST API, not
workflow syntax, so plugin-level configuration preserves parity while
exposing Azure exact-match subjects and AWS session tags that inline GitHub
workflows cannot express.

## Security boundaries

- The token attests the Buildkite job identity. Any code in the destination
  job can mint the same token, which is the boundary the secrets integration
  already documents. This feature adds no new authority.
- Agent connection material stays out of action subprocess environments. The
  runtime holds the job token; actions receive only the loopback URL and a
  single-purpose random bearer token.
- Minted tokens never enter the initial job environment, plans, or generated
  pipeline YAML, and are registered with both redactors before first use.

## Scope and non-goals

- Host JavaScript and composite actions only. Docker container actions and
  job containers cannot reach the host loopback listener; jobs that grant
  `id-token: write` fail container action token requests with a clear
  diagnostic. Container reachability via the job Docker network is a
  possible follow-up, not slice 1.
- No GitHub-shaped claims (`repository`, `workflow`, `job`). A backend claim
  extension could add them later; trust policies work today with Buildkite's
  claim set.
- No injected or first-party `oidc-action`. Actions speak `getIDToken()`;
  the loopback service is the exposure surface, and injecting a step would
  require handing a credential to action code or new backend credentials.

## Delivery slices

### 1. Endpoint contract and minting client

Accept `id-token` in `adaptPermissions`, carry the grant in the plan, add
`AgentOIDCTokens`, and serve the loopback endpoint for host actions.
Update `docs/compatibility.md` (permissions table, "Other secrets and
OIDC"), `docs/security.md`, and the README support lists.

Verification:

- unit tests mirroring `github_token_service_test.go` against a fake agent
  endpoint, covering audience passthrough, auth failures, and malformed
  responses
- a runtime test where a Node step uses a contract-conformant shim mirroring
  `actions/toolkit`'s `oidc-utils.ts` against the real loopback service; this
  verifies the wire contract, not the upstream package
- parse tests flip the `id-token` rejection case to acceptance and gate
  variable absence without the permission
- `mise run check` stays network-free
- deferred hosted runtime proof: `aws-actions/configure-aws-credentials` completes a
  live `AssumeRoleWithWebIdentity` against an IAM provider trusting
  `agent.buildkite.com`

### 2. Plugin `oidc` configuration

Plugin block validated at compile time and applied to every mint in the job:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/deploy.yml
      oidc:
        claims: [organization_id]
        aws-session-tags: [organization_slug, pipeline_id]
        subject-claim: pipeline_id
```

Deliver when a workflow needs AWS session tags or Azure exact-match
subjects.

## Open questions

- Whether the Agent API `oidc/tokens` endpoint accepts the job token this
  runtime already uses for `github_workflow_access_token` and cache minting,
  and whether organization OIDC policy should gate it. Backend confirmation
  resolves this before slice 1 merges; the endpoint shape is otherwise
  identical to the existing minting clients.
- Whether a future agent Job API `oidc/tokens` route should replace the
  direct Agent API call. It would not change this design: actions still
  speak `getIDToken()`, so the loopback translation layer remains either
  way.

## References

- `actions/toolkit` `packages/core/src/oidc-utils.ts` (endpoint contract)
- `buildkite/agent` `api/oidc.go`, `clicommand/oidc_request_token.go`
- [Buildkite agent OIDC reference](https://buildkite.com/docs/agent/cli/reference/oidc)
- [Compatibility reference](../compatibility.md#other-secrets-and-oidc)
- [Security model](../security.md)
