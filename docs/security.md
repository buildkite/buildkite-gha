# Security model

GitHub Actions normally combines workflow execution and GitHub-managed
credentials behind a runner. `buildkite-gha` keeps the workflow syntax but runs
each supported job as a native Buildkite job. It does not create a GitHub
Actions run or a GitHub-hosted runner, so Buildkite's agent, queue, and policies
provide the security boundary.

Treat workflow steps and third-party actions as you would on a self-hosted
GitHub Actions runner: code can use anything available to its job. Workflow
syntax can request permissions, but it does not make code trusted or grant
access by itself.

## How GitHub Actions security maps to Buildkite

| GitHub Actions concept | `buildkite-gha` and Buildkite boundary |
| --- | --- |
| Runner or runner group | The Buildkite queue selects the agent environment. Use a disposable host or equivalent whole-job isolation. |
| Job | A native Buildkite command job. All workflow steps share its workspace and Buildkite identity. |
| `permissions` and `GITHUB_TOKEN` | Top-level workflow permissions and Buildkite's workflow-token policy determine whether Buildkite issues a scoped token. GitHub repository and organization defaults are not inherited. |
| Repository and environment secrets | Static secret names resolve through Buildkite Secrets when the destination job's identity and Secret access policy allow them. GitHub environment, event, and fork scoping are not inherited. |
| OIDC | Actions use Buildkite-issued tokens and claims. Cloud trust policies must trust Buildkite rather than GitHub. |

The [compatibility reference](compatibility.md) says what works. This page says
where trust and authorization come from.

## Isolate the whole job

Steps in one job share:

- the workspace
- environment changes
- running processes
- action state
- the job's Buildkite identity

A shell step can affect a later action, and an action can affect a later shell
step. Docker actions, job containers, and service containers add packaging;
they are not security boundaries.

Run untrusted jobs on a queue with:

- a disposable machine or equivalent whole-job isolation
- no ambient protected credentials
- a clean environment for every job
- host-level CPU, memory, disk, and network limits

Service options can grant privileges, mount host paths, and publish ports. The
private Docker network, ownership labels, and cleanup checks reduce accidental
residue. They do not contain hostile code.

On a persistent self-hosted agent, workflow code can read exposed host
resources and leave state for later jobs.

## Repository data does not grant authority

Treat workflow files, action metadata, event snapshots, and job plans as
untrusted input. They can describe work and request permissions. Buildkite
configuration and server-side policy choose the queue and decide which
credentials the job can receive.

Digests and immutable source locks detect changed code. They do not make code
trusted or grant credentials.

Prebuilt Docker action metadata can name a public `docker://` image. The action
source lock protects the image declaration, but a mutable image tag can resolve
to different content when each job starts. Use an image digest when content
immutability matters. Image pulls use an empty private Docker configuration and
never receive action secrets, registry credentials, or ambient Docker
authority; private images are unsupported.

### Source and event checks

- Public actions and reusable workflows resolve once per operation to an
  immutable commit and repository digest.
- Plans bind the digest of each selected workflow file.
- Runtime jobs do not load remote workflow YAML from the caller workspace.
- Path-filter admission uses reserved linked-webhook data only after matching
  it to the Buildkite repository, commit, workflow, and bounded local Git
  history. Missing, shallow, ambiguous, or mismatched evidence blocks
  admission.
- Release ingestion matches the webhook activity and tag to Buildkite's event,
  branch, and tag. The GitHub Code Access App supplies server-resolved commit
  provenance. A local `HEAD` fallback preserves compatibility but cannot grant
  hosted release token issuance.

Explicit and generated event snapshots provide compatibility context. They do
not authorize path-filter admission, queues, secrets, or tokens.

### Reusable-workflow guards

Reusable-workflow call conditions become immutable plan guards. They run in the
caller scope before the flattened job requests secrets or tokens, starts OIDC,
materializes actions, creates containers, or runs steps.

Direct `needs` values come from producer-attributed, digest-bound result
manifests. A missing or changed manifest stops the job.

## Credential boundaries

| Credential | Boundary |
| --- | --- |
| Repository checkout | The native adapter checks the event repository and exact commit. Buildkite authorizes private access. Credentials apply only to Git commands and are not persisted. |
| `GITHUB_TOKEN` | A short-lived token for the event repository. Buildkite enforces the top-level workflow permission map and build provenance. The token is not ambient. |
| Cache token | A fresh job-bound token for each compatible JavaScript or Docker action lifecycle. Shell steps do not receive it. |
| Workflow secrets | Static names resolve with `buildkite-agent secret get` in the destination job. Buildkite Secret access policy is the authority. |
| Registry credentials | Explicit credentials resolve in the destination job. Passwords go to Docker through standard input and use a private per-job Docker configuration. Secret-derived values stay out of plans and pipeline YAML; authored literals do not. |
| OIDC token | Host JavaScript actions in jobs with `id-token: write` can request Buildkite OIDC tokens through a loopback endpoint. Shell steps and containerized actions cannot. |

An action that receives a credential can use or exfiltrate it. It can also
export `GITHUB_TOKEN` to later steps through `GITHUB_ENV`. Masking reduces
accidental disclosure; it is not access control and cannot catch transformed
values.

### GitHub token

Token issuance requires a Buildkite organization feature and a default-off
pipeline setting. Buildkite reads only the top-level workflow permission policy
from the pipeline repository at the build's immutable commit.

Important limits:

- Omitted permissions mean exactly `contents: read`; GitHub repository and
  organization defaults are not inherited.
- Write access requires an explicit top-level map.
- `read-all` and `write-all` expand to the 13 supported repository scopes. They
  do not include `id-token` or unsupported aliases.
- An empty map, or a map containing only `none`, creates no token.
- Job-level and called-workflow repository maps do not narrow or expand the
  token. Reusable jobs use the top-level requesting workflow's permissions.
- Pull requests have a `contents: read` ceiling. Merge queues are denied.
- Incomplete or cyclic trigger and rebuild provenance is denied.
- Native release builds need the GitHub Code Access App for server-side commit
  provenance.

The exact step call `toJSON(github)` also requests a token because the retained
context includes `token`. Before evaluating an authorized step context, the
runtime registers the token with both Buildkite Agent redaction and local
redaction.

The serialized context contains only the fields listed in the
[compatibility reference](compatibility.md#runtime-interpolation). It does not
contain `github.event` or the full event payload.

For non-pull-request builds, a user who can create a build at any commit may
choose code that requests the workflow's allowed permissions. Enable write
tokens only when those build-creation paths are trusted.

### Workflow secrets

Workflow secrets are Buildkite secrets available to the destination job. They
are not GitHub repository, environment, event, or fork-scoped secrets.

`secrets: inherit` lets a local reusable-workflow call place that callee job's
statically referenced secret names in its plan. It is one hop, and every nested
edge must repeat it. A local call can instead map a declared callee alias from
one direct caller secret reference. Required declarations must be mapped;
optional unmapped aliases stay empty.

Nested explicit mappings compose aliases to the original Buildkite secret.
They can forward only authority received from the parent and never fall back to
a same-named Buildkite secret. The runtime retrieves each original once and
projects its value to the authorized aliases. Remote forwarding and secret
names invented by action metadata remain unsupported.

Plans and pipeline YAML contain secret names, never values. Restrict the
destination job with Buildkite Secret access policies. Arbitrary code in the
same job identity can also run `buildkite-agent secret get`.

`GITHUB_TOKEN` stays on its separate workflow-token boundary. Forwarding it to
a declared alias preserves that scoped token boundary; it never requests an
ordinary Buildkite secret.

### OIDC

Jobs with `id-token: write` expose the `getIDToken()` wire contract to each host
JavaScript action invocation. The endpoint is loopback-only and protected by a
single-purpose bearer token. The runtime mints a Buildkite token for the
action's requested audience, then registers it with both redactors before use.

Cloud providers must trust Buildkite's issuer and claims. GitHub-shaped claims
and GitHub's issuer are not emulated. Plugin OIDC configuration can add
Buildkite claims without granting `id-token: write`.

## Checkout and submodules

Buildkite authorizes managed GitHub and Origin repositories through Git's
credential protocol. A checked-in `.gitmodules` file can select another
repository from the same provider when Buildkite authorizes it.

- Managed tokens are repository-specific and read-only.
- External HTTPS submodules are anonymous.
- SSH and other non-HTTPS transports are disabled.
- The credential helper is offered only to the event provider's host, uses
  HTTP-path matching, and is not persisted.

Git owns submodule parsing and recursion, so keep it current and prefer a pinned
job image.

Command scoping limits accidental spread. It does not stop a hostile concurrent
process under the same job identity from reaching the agent or credential
helper. Use a separate UID, sandbox, or pre-job credential broker for that
boundary.

## Operator checklist

1. Leave the plugin `version` unset for the latest stable release, or pin an
   exact stable release from `0.8.0` onward for a controlled rollout.
1. Run imported jobs on an isolated queue with no ambient credentials.
1. Treat public actions as third-party code and prefer immutable commit pins.
1. Restrict managed repository access, secrets, and write tokens with Buildkite
   policy.
1. Keep Git and the job image patched.
1. Validate before upload:

    ```sh
    buildkite-gha validate \
      --profile hosted \
      --event-path event.json \
      .github/workflows/ci.yml
    ```

1. Keep private actions and protected queues out of imported workflows.
1. Configure OIDC trust for Buildkite's issuer, then restrict subjects and
   audiences to the intended jobs.
