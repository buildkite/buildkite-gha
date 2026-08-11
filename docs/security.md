# Security model

`buildkite-gha` runs workflow steps and third-party actions as native Buildkite
jobs. Treat that code like any other repository script: it is not made safe by
using Actions syntax.

The [compatibility reference](compatibility.md) is the source of truth for
supported features. This page explains the trust boundaries around them.

## Isolate the whole job

The runtime does not isolate steps from each other. They share a workspace,
environment changes, processes, and action lifecycle. A shell step can affect a
later action, and an action can affect a later shell step.

Dockerfile actions add packaging, not a security boundary. When a queue uses a
disposable job VM, that VM—not the action container—is the boundary around the
Docker daemon and workflow code.

Run imported jobs on a queue that provides:

- whole-job isolation;
- no ambient protected credentials; and
- a clean machine for each untrusted job.

On a persistent self-hosted agent, workflow code can use whatever host resources
the agent process exposes. Files or processes it leaves behind may affect later
jobs.

## Inputs do not grant authority

Workflow files, action metadata, event snapshots, and webhook payloads are
untrusted inputs. They can request work, queues, and permissions, but are not
sufficient authority for them. Buildkite configuration and server-side policy
decide what is allowed.

Content digests and immutable action locks detect changed code. A digest alone
does not make that code trusted or grant it authority.

## Credential boundaries

| Credential | Supported boundary |
| --- | --- |
| Repository checkout | The verified adapter checks the plan's event repository and exact commit. Buildkite authorizes private repository access. Credentials are not persisted. |
| `GITHUB_TOKEN` | One short-lived token is requested for the plan's event repository and compiler-resolved permissions. Buildkite independently requires the pipeline repository to match and may deny the request. The token is not initially ambient. |
| Cache token | Fresh job-bound credentials are minted for each JavaScript or Docker action lifecycle when the cache service is configured. Ordinary shell steps do not receive them. |
| Ordinary workflow secrets | Rejected by production admission. |
| GitHub-compatible OIDC | Unsupported. |

### Checkout and submodules

Checked-in `.gitmodules` files may select repositories covered by the job's
Buildkite managed code access. For GitHub, that can include another repository
in the same account when the connected GitHub App installation includes it.
Buildkite authorizes each repository and returns a repository-specific,
read-only token. The helper is offered only to `github.com`, is scoped by HTTP
path, and is not persisted. External HTTPS submodules are fetched anonymously;
SSH and non-HTTPS transports are disabled.

The installed Git executable owns submodule manifest parsing, paths, relative
URLs, and recursion. Use a current, vendor-supported Git distribution,
preferably pinned in an immutable job image.

Job binding does not prove that a branch, actor, or fork is trusted. If an
untrusted change can edit a workflow that requests write permissions, that code
can use the resulting token. Restrict write permissions with Buildkite
organization and pipeline policy.

An action that receives a credential has the same ability to use or exfiltrate
it as a shell command in that job. It can also export `GITHUB_TOKEN` to later
steps through `GITHUB_ENV`. Log masking hides registered literal values; it is
not access control and cannot detect every encoded or transformed value.

Checkout's command-scoped environment and Git configuration reduce accidental
credential spread; they do not isolate the helper or Agent identity from a
hostile concurrent process under the same job identity. Protecting checkout
credentials from hostile same-job code would require a separate UID, sandbox,
or pre-job credential broker rather than more `.gitmodules` parsing.

## Operator checklist

1. Pin a released plugin version.
2. Use an isolated queue with no ambient credentials.
3. Treat public actions as third-party code; prefer immutable commit pins.
4. Restrict repository credentials and write tokens with Buildkite policy.
5. Keep the queue's Git distribution current and patched.
6. Validate the production profile before upload:

   ```sh
   buildkite-gha validate \
     --profile hosted-tokenless \
     --event-path event.json \
     .github/workflows/ci.yml
   ```

7. Keep unsupported secrets, private actions, OIDC, and protected queues out of
   imported workflows.

For implementation detail, see the proposed
[protected-capability control-plane ADR](architecture/0003-protected-capability-control-plane.md).
