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

## Repository data is not authority

Workflow files, action metadata, event snapshots, and job plans are untrusted inputs. They may describe work and request permissions. Buildkite configuration and server-side policy choose the queue and decide what authority is available.

Digests and immutable action locks detect changed code. They do not make code trusted or grant credentials.

## Credential boundaries

| Credential | Current boundary |
| --- | --- |
| Repository checkout | The verified adapter checks the event repository and exact commit. Buildkite authorizes managed private access; credentials are command-scoped and not persisted. |
| `GITHUB_TOKEN` | Supported static uses receive one short-lived token for the event repository and compiler-resolved permissions. Omitted workflow permissions mean exactly `contents: read`; GitHub repository and organization settings are not inherited. Buildkite verifies the pipeline repository, immutable commit, workflow policy, and build provenance. Pull requests are limited to `contents: read`; merge queues are denied. The token is not ambient. |
| Cache token | When caching is configured, every JavaScript or Docker action lifecycle receives a fresh job-bound token. This includes compatible clients such as `actions/setup-go`, not only `actions/cache`. Shell steps do not receive it. |
| Ordinary workflow secrets | Rejected by production admission. |
| GitHub-compatible OIDC | Unsupported. |

An action that receives a credential can use or exfiltrate it. It can also export `GITHUB_TOKEN` to later steps through `GITHUB_ENV`. Log masking reduces accidental disclosure, but it is not access control and does not catch transformed values.

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

1. Keep unsupported secrets, private actions, OIDC, and protected queues out of imported workflows.
