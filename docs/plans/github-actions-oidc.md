# GitHub Actions OIDC compatibility

## Status

Host JavaScript and composite actions can request Buildkite OIDC tokens from a
job with `permissions: id-token: write`. The compiler, job plan, minting client,
loopback service, and plugin configuration are delivered.

The [compatibility reference](../compatibility.md#other-secrets-and-oidc) owns
the supported workflow syntax. The [security model](../security.md) owns the
credential boundary.

## Original problem

Cloud authentication actions call `actions/core.getIDToken(audience)`. They do
not contact GitHub directly. The toolkit reads two environment variables,
sends a bearer-authenticated request, and expects:

```json
{"value": "<jwt>"}
```

Earlier versions of `buildkite-gha` rejected the `id-token` permission. The
runtime now implements the toolkit's endpoint contract, so supported actions
can run without workflow changes.

Cloud trust configuration still changes: providers must trust Buildkite's
issuer and claims, not GitHub's.

## Request flow

```text
Host JavaScript action
  │ GET /idtoken?api-version=2&audience=<audience>
  │ Authorization: Bearer <single-purpose token>
  ▼
Per-job loopback service
  │ POST /jobs/<job-id>/oidc/tokens
  │ Authorization: Token <Buildkite job token>
  ▼
Buildkite Agent API
```

The loopback service starts only for jobs with `id-token: write`. Each action
invocation receives its own random bearer token and endpoint URL.

## Design decisions

### Permissions and plans

`id-token` is separate from repository permissions. It never reaches
`GITHUB_TOKEN` policy or grants repository access.

The job plan records whether OIDC is allowed. Without `id-token: write`, the
runtime omits the endpoint variables and the action reports the same
missing-variable error it would report on GitHub.

### Token minting

The runtime calls the Agent API with the job-scoped token it already holds. It
validates the API URL, bounds and validates the response, then checks that the
returned token has JWT shape.

An empty action audience selects Buildkite's default audience. The API's
default token lifetime is used.

### Exposure and redaction

Before returning a minted token, the runtime registers it with the Buildkite
Agent redactor and its local workflow-command redactor.

Action subprocesses receive only the loopback endpoint and its single-purpose
bearer token. Agent connection details and the Buildkite job token stay in the
runtime.

### Plugin configuration

Operators can apply extra Buildkite claims to every mint:

```yaml
plugins:
  - github-actions#latest:
      workflow: .github/workflows/deploy.yml
      oidc:
        claims: [organization_id]
        aws-session-tags: [organization_slug, pipeline_id]
        subject-claim: pipeline_id
```

This configuration changes token claims. It does not grant `id-token: write`.

## Scope

Supported:

- host JavaScript actions
- JavaScript actions called by composite actions
- action-selected audiences
- plugin-level Buildkite claims, AWS session tags, and subject claim

Not supported:

- shell steps
- Docker container actions
- actions running inside job containers
- GitHub-shaped claims such as `repository`, `workflow`, and `job`
- signing tokens as `token.actions.githubusercontent.com`

Any code in an authorized destination job already runs with that job's
identity. The loopback translation does not create a stronger identity.

## Verification

Repository coverage includes:

- permission parsing and plan transport
- audience forwarding and Agent API authentication
- malformed and failed Agent API responses
- endpoint availability only with `id-token: write`
- a contract-compatible Node shim against the real loopback service
- plugin OIDC configuration validation and forwarding

`mise run check` remains network-free. A hosted cloud-provider exchange is a
separate runtime proof.

## Open question

A future job-specific Agent API route could replace the current token endpoint.
The action-facing loopback contract would remain the same.

## References

- [Buildkite agent OIDC reference](https://buildkite.com/docs/agent/cli/reference/oidc)
- [Compatibility reference](../compatibility.md#other-secrets-and-oidc)
- [Security model](../security.md)
