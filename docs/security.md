# Security model

`buildkite-gha` runs workflow steps and third-party actions as native Buildkite
jobs. Actions syntax does not make that code trusted.

The [compatibility reference](compatibility.md) lists supported features. This
page explains the boundaries operators must provide around them.

## Isolate the whole job

Steps share a workspace, environment changes, processes, action state, and the
job's Buildkite identity. A shell step can affect a later action, and an action
can affect a later shell step.

Dockerfile actions add packaging, not a security boundary. Use a queue with:

- a disposable machine or equivalent whole-job isolation;
- no ambient protected credentials; and
- a clean environment for every untrusted job.

On a persistent self-hosted agent, workflow code can access exposed host
resources and leave state that affects later jobs.

## Repository data is not authority

Workflow files, action metadata, event snapshots, and job plans are untrusted
inputs. They may describe work and request permissions. Buildkite configuration
and server-side policy choose the queue and decide what authority is available.

Digests and immutable action locks detect changed code. They do not make code
trusted or grant credentials.

## Credential boundaries

| Credential | Current boundary |
| --- | --- |
| Repository checkout | The verified adapter checks the event repository and exact commit. Buildkite authorizes managed private access; credentials are command-scoped and not persisted. |
| `GITHUB_TOKEN` | Supported static uses receive one short-lived token for the event repository and compiler-resolved permissions. Buildkite verifies the repository and may deny the request. The token is not ambient. |
| Cache token | When caching is configured, every JavaScript or Docker action lifecycle receives a fresh job-bound token. This includes compatible clients such as `actions/setup-go`, not only `actions/cache`. Shell steps do not receive it. |
| Ordinary workflow secrets | Rejected by production admission. |
| GitHub-compatible OIDC | Unsupported. |

An action that receives a credential can use or exfiltrate it. It can also
export `GITHUB_TOKEN` to later steps through `GITHUB_ENV`. Log masking reduces
accidental disclosure; it is not access control and does not catch transformed
values.

If untrusted code can edit a workflow that requests write permissions, it can
use a token that Buildkite allows. Restrict write permissions with organization
and pipeline policy.

### Checkout and submodules

Buildkite authorizes every managed GitHub repository requested through Git's
credential protocol. Checked-in `.gitmodules` may select another repository in
the same GitHub account when the connected App installation includes it. Those
tokens are repository-specific and read-only. External HTTPS submodules are
anonymous; SSH and non-HTTPS transports are disabled.

The credential helper is offered only to `github.com`, uses HTTP-path matching,
and is not persisted. The installed Git executable owns submodule parsing and
recursion, so keep it current and preferably pin it in the job image.

Command scoping limits accidental spread. It does not stop a hostile concurrent
process under the same job identity from reaching the Agent or helper. That
requires a separate UID, sandbox, or pre-job credential broker.

## Operator checklist

1. Pin a released plugin version.
2. Run imported jobs on an isolated queue with no ambient credentials.
3. Treat public actions as third-party code and prefer immutable commit pins.
4. Restrict managed repository access and write tokens with Buildkite policy.
5. Keep Git and the job image patched.
6. Validate before upload:

   ```sh
   buildkite-gha validate \
     --profile hosted-tokenless \
     --event-path event.json \
     .github/workflows/ci.yml
   ```

7. Keep unsupported secrets, private actions, OIDC, and protected queues out of
   imported workflows.

Broader protected capabilities require the Buildkite-owned policy boundary in
[ADR 0003](architecture/0003-protected-capability-control-plane.md); repository
configuration alone cannot authorize them.
