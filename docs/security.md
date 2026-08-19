# Security model

`buildkite-gha` runs workflow steps and third-party actions as native Buildkite jobs. GitHub Actions syntax does not make that code trusted.

The [compatibility reference](compatibility.md) lists supported features. This page explains the boundaries that operators must provide around them.

## Isolate the whole job

Steps share a workspace, environment changes, processes, action state, and the job's Buildkite identity. A shell step can affect a later action, and an action can affect a later shell step.

Dockerfile actions add packaging, not a security boundary. Use a queue with:

- A disposable machine or equivalent whole-job isolation
- No ambient protected credentials
- A clean environment for every untrusted job

On a persistent self-hosted agent, workflow code can access exposed host resources and leave state that affects later jobs.

Job and service containers also share the job's Docker daemon and host resource budget. Service options can grant privileges, mount host paths, and publish ports on Docker-selected interfaces. Their private bridge network, ownership labels, and verified cleanup reduce accidental residue; they do not isolate hostile code or enforce network, CPU, memory, or disk limits. The queue and VM firewall must provide those boundaries around the whole job.

## Repository data is not authority

Workflow files, action metadata, event snapshots, and job plans are untrusted inputs. They may describe work and request permissions. Buildkite configuration and server-side policy choose the queue and decide what authority is available.

Digests and immutable source locks detect changed code. They do not make code trusted or grant credentials.

Public reusable workflows use the same bounded repository source and cache as public actions. Each requested ref resolves once per validation, compilation, or upload operation to an immutable commit and repository digest. Plans also bind each selected workflow file digest. Runtime jobs do not load remote workflow YAML from the caller workspace.

Push and pull request path-filter admission uses Buildkite's reserved linked-webhook metadata only after binding it to the Buildkite repository and commit and matching local Git history. Missing, shallow, ambiguous, oversized, or mismatched evidence prevents admission. Explicit and generated snapshots cannot grant this admission. This check controls workflow selection; it does not make the selected workflow trusted.

Release ingestion also requires reserved linked-webhook metadata. It binds the webhook activity to `BUILDKITE_GITHUB_ACTION`, the release tag to both `BUILDKITE_TAG` and `BUILDKITE_BRANCH`, and the event SHA to the checked-out commit after the plugin resolves Buildkite's symbolic `HEAD`. Environment fallback cannot invent a release event. Enable **Additional Webhooks** > **Releases** only with **Code** trigger mode.

Reusable-workflow call conditions are immutable plan guards evaluated in caller scope. Direct `needs` values come only from producer-attributed, digest-bound result manifests; a missing or changed manifest stops the job with an error. A false guard skips the flattened job before secret retrieval, workflow-token minting, OIDC startup, action materialization, containers, or steps.

## Credential boundaries

| Credential | Current boundary |
| --- | --- |
| Repository checkout | The verified adapter checks the event repository and exact commit. Buildkite authorizes managed private access; credentials are command-scoped and not persisted. |
| `GITHUB_TOKEN` | Supported static uses receive one short-lived token for the event repository and top-level requesting workflow permissions. The exact step-runtime call `toJSON(github)` counts as a token use because the bounded serialized context includes `token`. Omitted workflow permissions mean exactly `contents: read`; GitHub repository and organization settings are not inherited. Reusable-workflow jobs receive the same permissions because Buildkite does not inspect called workflow permission maps. Buildkite verifies the pipeline repository, immutable commit, top-level workflow policy, and build provenance. Pull requests are limited to `contents: read`; merge queues are denied. The token is not ambient. |
| Cache token | When caching is configured, every JavaScript or Docker action lifecycle receives a fresh job-bound token. This includes compatible clients such as `actions/setup-go`, not only `actions/cache`. Shell steps do not receive it. |
| Ordinary workflow secrets | Static names are resolved with `buildkite-agent secret get` in the destination job. The job's Buildkite identity and Secret access policies are the sole authorization boundary. Values are registered with Agent and local redaction before use. |
| Service registry credentials | Explicit credentials can be literal or use supported `github`, `vars`, `secrets`, and `env` expressions. Ordinary secrets use the destination job's Buildkite Secret access policy. Secret-derived values stay out of plans and generated pipeline YAML; authored literal values do not. Passwords pass to Docker through standard input. Authentication uses a private per-job Docker configuration that cleanup verifies is removed. Ambient Docker configuration and implicit GHCR authentication are unavailable. |
| OIDC token | Jobs with `id-token: write` expose the `getIDToken()` wire contract through a per-invocation, bearer-authenticated loopback endpoint to host JavaScript actions. It mints Buildkite OIDC tokens for action-requested audiences. Tokens use Buildkite's issuer and claims and are registered with Agent and local redaction before use. Shell steps and containerized actions do not receive the endpoint. Repository tests use a contract-conformant shim; the hosted runtime proof remains pending. |

An action that receives a credential can use or exfiltrate it. It can also export `GITHUB_TOKEN` to later steps through `GITHUB_ENV`. Log masking reduces accidental disclosure, but it is not access control and does not catch transformed values.

The runtime registers a workflow token with Buildkite Agent and local redaction before evaluating an authorized step context. This also masks the token when `toJSON(github)` embeds it in logs, errors, outputs, results, summaries, or annotations. The call exposes only the retained GitHub fields documented in the compatibility reference, not `github.event` or the full event payload. Job-level contexts and action metadata input defaults remain tokenless.

Ordinary workflow secrets are Buildkite job-accessible secrets, not GitHub event or fork-scoped secrets. Workflow syntax adds no authority: arbitrary workflow code already runs with the destination job's identity and can call `buildkite-agent secret get`. Restrict that identity with Buildkite Secret access policies. Plans and generated pipeline YAML contain secret names only, never values. `GITHUB_TOKEN` remains on its separate workflow-token boundary.

Workflow token issuance requires an organization feature and a default-off pipeline setting. For `GITHUB_TOKEN`, Buildkite reads and enforces only the top-level repository permission policy from the requesting workflow at the build's immutable commit. Omitted permissions resolve to exactly `contents: read`; write permissions require an explicit top-level map. Top-level `read-all` becomes an explicit map of the 13 supported read scopes in plans and token requests. It excludes `id-token`, `models`, `repository-projects`, `code-quality`, `metadata`, and `vulnerability-alerts`; no alias crosses the wire. Server support for validating the immutable source policy must be deployed before the supporting client. Explicit empty top-level permissions, top-level scopes resolving only to `none`, `write-all`, and job-level aliases cannot receive a token. Job-level and called-workflow repository permission maps do not narrow or expand `GITHUB_TOKEN`; the separate OIDC credential boundary retains job-level and called-workflow `id-token` narrowing. Buildkite denies incomplete or cyclic trigger and rebuild provenance. Pull-request ancestry retains a `contents: read` ceiling, and merge-queue ancestry is denied.

For other builds, a user with permission to create a build at an arbitrary commit may select code that requests the workflow's allowed permissions. Enable write tokens only when those build-creation paths and branch builds are trusted.

### Checkout and submodules

Buildkite authorizes managed GitHub and Origin repositories requested through the Git credential protocol. A checked-in `.gitmodules` file may select another repository from the same provider when Buildkite authorizes access to it. Those tokens are repository-specific and read-only. External HTTPS submodules are anonymous. SSH and non-HTTPS transports are disabled.

The credential helper is offered only to the event provider's host (`github.com` or `origin.cursor.com`), uses HTTP-path matching, and is not persisted. The installed Git executable owns submodule parsing and recursion, so keep it current and preferably pin it in the job image.

Command scoping limits accidental spread. It does not stop a hostile concurrent process under the same job identity from reaching the agent or helper. That requires a separate UID, sandbox, or pre-job credential broker.

## Operator checklist

1. Leave the plugin's CLI `version` unset to follow the latest stable `buildkite-gha` release, or set an exact stable release from `0.8.0` onward when a controlled rollout requires a pin.
1. Run imported jobs on an isolated queue with no ambient credentials.
1. Treat public actions as third-party code and prefer immutable commit pins.
1. Restrict managed repository access and write tokens with Buildkite policy.
1. Keep Git and the job image patched.
1. Validate before upload:

    ```sh
    buildkite-gha validate \
      --profile hosted \
      --event-path event.json \
      .github/workflows/ci.yml
    ```

1. Keep private actions and protected queues out of imported workflows. Configure OIDC trust for Buildkite's issuer and restrict subjects and audiences to the intended jobs.
