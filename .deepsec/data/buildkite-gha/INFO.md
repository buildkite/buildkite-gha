# buildkite-gha

## What this codebase does

`buildkite-gha` is a Go CLI that compiles a supported subset of GitHub Actions
workflow YAML into native Buildkite jobs, then executes each job's ordered
steps in a compatibility runtime. The compiler resolves public actions,
constructs content-addressed job plans, applies production admission policy,
and uploads plans plus generated pipeline YAML through `buildkite-agent`.
`run-job` independently validates a plan, rechecks its workflow digest, and
executes workflow-controlled shell, JavaScript, composite, checkout, artifact,
cache, and narrowly supported Docker action behaviour.

## Auth shape

- `hosted-tokenless` admission is the production authority boundary. Fresh
  compiler `PlanAuthorization` evidence decides whether requested Docker or
  provider-token capabilities may enter an uploaded plan.
- `plan.Job.Validate`, `verifyWorkflow`, and runtime capability checks bind a
  job to exact workflow bytes and fail closed on unknown authority.
- `resolveAgentRepositoryCredentialsBeforeWorkflow` and
  `runRepositoryProviderCheckoutFetch` confine the Buildkite Agent access token
  to Git fetches through the repository-provider credential helper.
- `resolveWorkflowToken` mints one event-repository token with
  compiler-resolved permissions; `resolveSecrets` loads only explicitly
  declared secret names. Both require the Buildkite Agent redactor before use.
- `cacheActionEnvironment` obtains fresh job-bound cache credentials for action
  lifecycles. Shell steps do not receive cache credentials.

## Threat model

Workflow files, action metadata, event/webhook JSON, job plans, action archives,
dependency results, and downloaded artifacts are untrusted. The highest-impact
attack is turning repository-controlled data into authority outside the
declared job: obtaining Buildkite credentials, minting broader GitHub access,
reading another repository, selecting a protected queue, escaping workspace or
archive roots, or modifying uploaded plans after admission. Workflow commands
and third-party actions intentionally execute arbitrary code, so isolation of
the whole job is an operator responsibility rather than an in-process sandbox.

Hosted jobs run on disposable machines, and the ephemeral job VM is the
security boundary. Steps, actions, Docker containers, and background processes
inside one VM share a workspace and job identity and may inspect or affect one
another. A finding must cross that VM boundary, obtain authority beyond the
admitted job, or persist sensitive data outside the VM. Per-step credential
delivery reduces accidental exposure; it is not isolation from other code in
the same VM.

## Project-specific patterns to flag

- Any path from workflow/event/plan fields to queue selection or
  `RequiredCapabilities` that bypasses `hosted-tokenless` admission or relies
  on encoded-plan claims instead of same-process compiler evidence.
- Any Buildkite Agent access token, repository credential, GitHub token,
  workflow secret, or cache token persisted in plans, pipeline YAML, logs,
  annotations, artifacts, caches, or other state that survives the job VM; or
  minted with broader repository or permission authority than admitted.
- Any checkout or submodule change that weakens exact event repository/SHA
  binding, `checkoutGitBaseArgs`, Git protocol restrictions, HTTP-path
  credential matching, or host executable pinning before workflow execution.
- Any artifact/action archive or workspace path operation that skips clean
  relative-path checks, `openat`/`O_NOFOLLOW`-style traversal, entry and byte
  limits, case-collision checks, or revalidation immediately before use.
- Any interpolation of webhook payload, workflow expressions, command files,
  action metadata, or Buildkite metadata into generated pipeline YAML, shell
  syntax, Git configuration, Docker arguments, annotations, or agent commands
  without the existing typed/argv and bounded-data boundaries.

## Known false-positives

- `run` steps and third-party actions intentionally execute
  repository-controlled code; findings must show a boundary escape or excess
  authority, not merely command execution.
- `GITHUB_ENV`, `GITHUB_PATH`, `GITHUB_OUTPUT`, and action state intentionally
  carry workflow-controlled values to later steps in the same job. They are not
  cross-job trust boundaries.
- Verified Dockerfile actions are supported in production and intentionally
  bind-mount the caller workspace and runner temp read-write. Docker is
  packaging, not a sandbox. The production profile rejects job and service
  containers.
- Same-job visibility through process arguments, process environments, shared
  files, or background processes is inside the VM boundary. Flag it only when
  it grants excess authority or sends durable sensitive data outside the VM.
- Test fixtures use obvious placeholder secret values to prove redaction and
  non-retention. They are not production credentials.
- GitHub API and archive downloads in `internal/action/source` intentionally
  access public action source; findings should identify a bypass of immutable
  commit locking, URL restrictions, or archive validation.
