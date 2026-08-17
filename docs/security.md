# Security model

GitHub Actions normally combines workflow execution and GitHub-managed credentials behind a runner. `buildkite-gha` keeps the workflow syntax but runs each supported job as a native Buildkite job. It does not create a GitHub Actions run or a GitHub-hosted runner, so Buildkite's agent, queue, and policies provide the security boundary.

Treat workflow steps and third-party actions as you would on a self-hosted GitHub Actions runner: code can use anything available to its job. Workflow syntax can request permissions, but it does not make code trusted or grant access by itself.

## How GitHub Actions security maps to Buildkite

| GitHub Actions concept | `buildkite-gha` and Buildkite boundary |
| --- | --- |
| Runner or runner group | The Buildkite queue selects the agent environment. Use a disposable host or equivalent whole-job isolation. |
| Job | A native Buildkite command job. All workflow steps share its workspace and Buildkite identity. |
| `permissions` and `GITHUB_TOKEN` | Top-level workflow permissions and Buildkite's workflow-token policy determine whether Buildkite issues a scoped token. GitHub repository and organization defaults are not inherited. |
| Repository and environment secrets | Static secret names resolve through Buildkite Secrets when the destination job's identity and Secret access policy allow them. GitHub environment, event, and fork scoping are not inherited. |
| OIDC | Actions use Buildkite-issued tokens and claims. Cloud trust policies must trust Buildkite rather than GitHub. |

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

Digests and immutable action locks detect changed code. They do not make code trusted or grant credentials.

Push and pull request path-filter admission uses Buildkite's reserved linked-webhook metadata only after binding it to the Buildkite repository and commit and matching local Git history. Missing, shallow, ambiguous, oversized, or mismatched evidence fails closed. Explicit and generated snapshots cannot grant this admission. This check controls workflow selection; it does not make the selected workflow trusted.

## Credential boundaries

| Credential | Current boundary |
| --- | --- |
| Repository checkout | The verified adapter checks the event repository and exact commit. Buildkite authorizes managed private access; credentials are command-scoped and not persisted. |
| `GITHUB_TOKEN` | Supported static uses receive one short-lived token for the event repository and compiler-resolved permissions. Omitted workflow permissions mean exactly `contents: read`; GitHub repository and organization settings are not inherited. Buildkite verifies the pipeline repository, immutable commit, workflow policy, and build provenance. Pull requests are limited to `contents: read`; merge queues are denied. The token is not ambient. |
| Cache token | When caching is configured, every JavaScript or Docker action lifecycle receives a fresh job-bound token. This includes compatible clients such as `actions/setup-go`, not only `actions/cache`. Shell steps do not receive it. |
| Ordinary workflow secrets | Static names are resolved with `buildkite-agent secret get` in the destination job. The job's Buildkite identity and Secret access policies are the sole authorization boundary. Values are registered with Agent and local redaction before use. |
| Service registry credentials | Explicit credentials can be literal or use supported `github`, `vars`, `secrets`, and `env` expressions. Ordinary secrets use the destination job's Buildkite Secret access policy. Secret-derived values stay out of plans and generated pipeline YAML; authored literal values do not. Passwords pass to Docker through standard input. Authentication uses a private per-job Docker configuration that cleanup verifies is removed. Ambient Docker configuration and implicit GHCR authentication are unavailable. |
| OIDC token | Jobs with `id-token: write` expose the `getIDToken()` wire contract through a per-invocation, bearer-authenticated loopback endpoint to host JavaScript actions. It mints Buildkite OIDC tokens for action-requested audiences. Tokens use Buildkite's issuer and claims and are registered with Agent and local redaction before use. Shell steps and containerized actions do not receive the endpoint. Repository tests use a contract-conformant shim; the hosted runtime proof remains pending. |

An action that receives a credential can use or exfiltrate it. It can also export `GITHUB_TOKEN` to later steps through `GITHUB_ENV`. Log masking reduces accidental disclosure, but it is not access control and does not catch transformed values.

Ordinary workflow secrets are Buildkite job-accessible secrets, not GitHub event or fork-scoped secrets. Workflow syntax adds no authority: arbitrary workflow code already runs with the destination job's identity and can call `buildkite-agent secret get`. Restrict that identity with Buildkite Secret access policies. Plans and generated pipeline YAML contain secret names only, never values. `GITHUB_TOKEN` remains on its separate workflow-token boundary.

Workflow token issuance requires an organization feature and a default-off pipeline setting. Buildkite reads the top-level permission policy from the workflow at the build's immutable commit. Omitted permissions resolve to exactly `contents: read`; write permissions require an explicit top-level map. Explicit empty permissions, scopes resolving only to `none`, job-level permission maps, and reusable-workflow jobs cannot receive a token. It denies incomplete or cyclic trigger and rebuild provenance. Pull-request ancestry retains a `contents: read` ceiling, and merge-queue ancestry is denied.

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
