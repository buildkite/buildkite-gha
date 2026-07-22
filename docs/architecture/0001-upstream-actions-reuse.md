# ADR 0001: Reuse actionlint as a parser and keep act as an oracle

- Status: Accepted
- Date: 2026-07-22
- Decision owners: `buildkite-gha` maintainers

## Context

`buildkite-gha` needs a GitHub Actions syntax frontend and a compatible job
runtime, but its durable interfaces are a provider-neutral workflow model and a
versioned Buildkite job plan. Reusing an upstream execution model across that
boundary would couple compilation, policy, action resolution, and execution to
the assumptions of a local Actions runner.

This spike inspected the current released source of `nektos/act`,
`rhysd/actionlint`, and GitHub's open runner. It considered workflow and action
models, matrix expansion, expression evaluation, resolution and execution,
workflow commands and environment files, dependency licensing, and version
pinning. The repositories were cloned and inspected locally as well as checked
against their release pages and documentation.

## Decision

Import `github.com/rhysd/actionlint` unchanged as a pinned parser frontend. Use
its workflow AST, source positions, expression lexer/parser/AST, availability
metadata, and, where useful, its static semantic checks. Immediately adapt the
parsed result into owned internal workflow and expression interfaces; neither
the actionlint AST nor any act type may appear in the versioned job plan.

Do not import an act package into production in the first implementation wave.
Use act's model, matrix, expression, action resolution, lifecycle, command, and
environment-file code as behavioral reference and as a source of differential
test cases. Implement those product boundaries in owned packages against the
official runner's observable behavior.

Maintain no forks. A fork is justified only after a differential fixture proves
a release-blocking gap, an upstream contribution is not viable on the required
timeline, and a small adapter or owned implementation would create more
maintenance risk. Record such a change in a new ADR with the upstream issue,
patch delta, merge cadence, and exit condition.

## Component decisions

### Workflow and action models

Use actionlint's position-bearing workflow AST as the syntax frontend and adapt
it immediately into an owned model. `actionlint.Parse` is a documented library
API and its nodes retain one-based line and column positions, which supports
source-located compatibility diagnostics.

Do not use `act/pkg/model` as the canonical model. Its structs mix decoded
values with mutable execution results, some fields retain `yaml.Node` while
others discard source locations, and helpers mutate model state or terminate
through act's logging conventions. Importing the package also pulls schema,
Git, logging, and roughly thirty third-party modules into the compiler. Act's
`ReadAction` and action types remain useful references for `action.yml`
coverage, but action metadata must also be adapted into owned types.

### Matrix expansion

Implement matrix expansion in the owned compiler. Use
`act/pkg/model.Job.GetMatrixes` as a test-input source, not as a dependency: the
method combines decoding, mutation, Cartesian expansion, include/exclude
policy, and act-specific error behavior. It also turns a zero-result product
into one empty matrix entry, so the required empty-matrix behavior cannot be
accepted without a GitHub differential fixture.

The implementation must separately represent an unevaluated matrix expression,
a statically evaluated matrix value, and expanded instances. This preserves the
compiler/runtime phase boundary needed for deferred matrices and allows explicit
size and cardinality limits before expansion.

### Expression parsing and evaluation

Import actionlint's lexer, parser, expression AST, and static type checker
unchanged behind an internal adapter. Implement evaluation, coercion, context
availability, status functions, and resource limits in an owned evaluator.

Do not import `act/pkg/exprparser`. It gives useful reference behavior for
coercion, wildcard access, functions, and truthiness, but its public environment
contains act model types and its status functions walk an act `Run`, workflow,
and mutable job results. `hashFiles` also embeds local-filesystem behavior. That
boundary cannot represent the plan's distinct compile-time, job-start,
step-start, and job-output phases without carrying act runtime state inward.

### Action resolution and execution

Implement provider-neutral action resolution and an immutable source cache in
owned packages. Act's `runner.ActionCache` demonstrates a useful split between
resolving a ref and reading an archive, but it lives in `pkg/runner`; importing
that package compiles the Docker, go-git, container, logging, and execution
stack. Its default resolver is GitHub-shaped and its action lifecycle shares a
mutable `RunContext`, while `buildkite-gha` must bind resolution to provider
provenance, signed plans, archive limits, and queue policy.

Use act's pre/main/post, JavaScript, composite, Docker, service, and cleanup
paths as scenario references. Use the official runner as the compatibility
oracle for lifecycle ordering and observable semantics. Do not use the official
runner listener, server job-message model, or C# worker assemblies as runtime
dependencies.

### Workflow commands and environment files

Implement an owned streaming command processor and per-step environment-file
processor. Act's command parser is tied to `RunContext`, logrus, and its output
pipeline, while its environment-file readers retrieve files through a job
container abstraction and include limits such as a 1 GB scanner buffer that do
not match this project's bounded-input policy.

Match the official runner instead: it serializes command handling, uses a
case-insensitive command registry, gives each step fresh file-command paths,
blocks `NODE_OPTIONS` through `GITHUB_ENV`, and processes state, outputs, paths,
and summaries after the step. Differential fixtures must cover LF and CRLF,
heredocs, malformed delimiters, stop-command tokens, annotation escaping,
masking order, and the concurrent-stream masking race.

## Evidence

| Upstream | Package or source inspected | Revision inspected | Disposition |
| --- | --- | --- | --- |
| [rhysd/actionlint](https://github.com/rhysd/actionlint/releases/tag/v1.7.12) | [`github.com/rhysd/actionlint`](https://github.com/rhysd/actionlint/blob/v1.7.12/docs/api.md) workflow AST, `Parse`, expression lexer/parser/checker, action metadata | `v1.7.12`, `914e7df21a07ef503a81201c76d2b11c789d3fca` | Import unchanged behind adapters |
| [nektos/act](https://github.com/nektos/act/releases/tag/v0.2.89) | [`pkg/model`](https://github.com/nektos/act/blob/v0.2.89/pkg/model/workflow.go), including `ReadWorkflow`, `ReadAction`, and `GetMatrixes` | `v0.2.89`, `4f411281417e88660bea1c1a1749aa71ae0bd60f` | Behavioral reference only |
| nektos/act | [`pkg/exprparser`](https://github.com/nektos/act/blob/v0.2.89/pkg/exprparser/interpreter.go) | `v0.2.89`, `4f411281417e88660bea1c1a1749aa71ae0bd60f` | Behavioral reference and fixture source only |
| nektos/act | `pkg/runner` action cache, remote resolution, and pre/main/post execution | `v0.2.89`, `4f411281417e88660bea1c1a1749aa71ae0bd60f` | Pattern and fixture reference only |
| nektos/act | [`pkg/runner/command.go`](https://github.com/nektos/act/blob/v0.2.89/pkg/runner/command.go) and environment-file handling | `v0.2.89`, `4f411281417e88660bea1c1a1749aa71ae0bd60f` | Behavioral reference only |
| [actions/runner](https://github.com/actions/runner/releases/tag/v2.336.0) | [`ActionCommandManager.cs`](https://github.com/actions/runner/blob/v2.336.0/src/Runner.Worker/ActionCommandManager.cs) and [`FileCommandManager.cs`](https://github.com/actions/runner/blob/v2.336.0/src/Runner.Worker/FileCommandManager.cs) | `v2.336.0`, `98aabcd429c4e8402406c56ce2d26387fed3b9ce` | Primary behavioral oracle; no dependency |

At these revisions, `go test ./pkg/model ./pkg/exprparser` passes in act.
Actionlint's core package and command tests pass; its repository-wide test run
has one failure in the network-sensitive `scripts/generate-popular-actions`
bad-request test, outside the imported package. `go list -deps` confirms that
actionlint's single root package compiles its linter dependencies, while
`act/pkg/model` reaches go-git and related modules and `act/pkg/runner` adds the
Docker/Moby stack.

## Licensing and dependency consequences

Act, actionlint, and the official runner are MIT licensed. Importing or copying
substantial source requires retaining their copyright and license notices.
Actionlint's compiled dependency set at v1.7.12 is permissively licensed but
includes Apache-2.0 and BSD-family notices in addition to MIT; release builds
must generate and review a third-party notice and SBOM from the resolved module
graph.

Avoiding act as a module keeps its much wider transitive graph out of the
product. The inspected act graph includes Docker/Moby Apache-2.0 components and
`github.com/cyphar/filepath-securejoin` under MPL-2.0. MPL-2.0 is compatible
with this use but adds file-level source obligations if modified and
distributed, which is another reason not to inherit the runner stack without a
product need. Any future copied act code must carry act's MIT notice adjacent
to the derived source and be included in release notices.

This is an engineering license inventory, not legal approval. The release gate
remains an automated license scan over the exact shipped source and bundled
runtimes.

## Version policy

- Pin actionlint to the exact `v1.7.12` module version and commit through
  `go.mod` and `go.sum`; do not use `latest` or a floating branch. Its own API
  documentation says library consumers do not receive semantic-versioning
  guarantees, so every update requires parser-adapter golden tests and the
  workflow corpus.
- Accept actionlint's Go 1.25 minimum as an explicit foundation constraint for
  this revision. Reconsider the pin if the project chooses an older Go support
  floor rather than silently raising the toolchain.
- Record exact act and official-runner tags and commits with every imported
  differential fixture. They are test provenance, not Go dependencies. The
  latest runner release may be progressively deployed, so GitHub differential
  evidence must also capture the runner version observed in each run.
- Do not combine act's pinned actionlint v1.7.7 dependency with a direct current
  actionlint dependency. Keeping act out of `go.mod` avoids Go minimal-version
  selection silently testing act against a newer, non-semver library API than
  act declares.

## Consequences for the next implementation wave

1. Build one internal parser adapter around actionlint and convert syntax nodes
   into owned workflow/action types with source spans.
2. Implement static matrix expansion and expression evaluation behind owned
   interfaces, seeded by act tests and verified by GitHub differential fixtures.
3. Define provider resolution, immutable archives, runtime state, workflow
   commands, and environment files independently of act's `RunContext` and
   container interfaces.
4. Add dependency-update CI that runs parser golden tests, the smoke corpus,
   license inventory, and SBOM generation before changing the actionlint pin.
5. Keep an explicit oracle manifest containing the act and official-runner
   revisions so behavior changes are reviewed rather than absorbed implicitly.
