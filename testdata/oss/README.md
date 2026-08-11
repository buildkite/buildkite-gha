# Open-source workflow compatibility corpus

This corpus measures unchanged workflows from public repositories against
`buildkite-gha`. It complements the owned smoke fixtures: those prove specific
contracts, while these cases reveal compatibility gaps in ordinary workflows
that were not written for this project.

Each manifest entry pins the source repository commit and workflow path. The
harness downloads only that raw workflow and verifies its checked-in SHA-256.
It does not clone the repository or execute third-party code. This deliberately
limits the corpus to workflows that do not need repository-local actions or
reusable workflows during compilation.

The initial ten cases deliberately mix outcomes:

| Case | Ecosystem/shape | Exact GitHub run | Current boundary |
| --- | --- | --- | --- |
| `urfave-cli-lint` | Go setup and third-party lint action | [Success](https://github.com/urfave/cli/actions/runs/29794452917) | Admitted by profile evaluation; runtime remains unproven. |
| `fastify-markdown-lint` | Small Node lint job with pinned actions | — | Admitted by profile evaluation; runtime remains unproven. |
| `prettier-lint` | Larger Yarn, cache, condition, and concurrency workflow | [Success](https://github.com/prettier/prettier/actions/runs/31316608109) | Admitted with ignored concurrency-cancellation behavior; runtime remains unproven. |
| `bat-changelog` | Pull-request shell and output workflow | — | Admitted; runtime needs a faithful real PR event. |
| `p-map-main` | Two-entry Node matrix | [Success](https://github.com/sindresorhus/p-map/actions/runs/29779845179) | Admitted by profile evaluation; runtime remains unproven. |
| `fzf-linux` | Go, Ruby, multiple shells, and host package setup | [Success](https://github.com/junegunn/fzf/actions/runs/31253573599) | Admitted; checkout's requested full history is supported. Runtime remains unproven. |
| `jq-valgrind` | C/autotools/Valgrind plus failure artifact | [Success](https://github.com/jqlang/jq/actions/runs/30182447884) | Profile-admitted with bounded direct submodules; emits `W_ACTION_RUNTIME_UNKNOWN` because static action metadata cannot prove independence from every GitHub-only runtime service. The profile does not execute the checkout or build. |
| `go-task-ci` | Five-job Go matrix | [Success](https://github.com/go-task/task/actions/runs/31330279154) | Compound concurrency expression is unsupported. |
| `gum-build` | Remote reusable workflow | — | Remote reuse and runtime secret forwarding are unsupported. |
| `just-ci` | Rust and a mixed-OS matrix | [Success](https://github.com/casey/just/actions/runs/31140381397) | Non-Linux runner rows are unsupported. |

Run the networked compile corpus with:

```sh
mise run corpus:oss
```

This prints every result, reports a passing/blocked summary, and exits non-zero
while any workflow is not compilable. Seven cases also retain a successful
upstream GitHub run at the exact source SHA as a future comparison reference.
Compiler determinism remains covered by the repository-owned smoke fixtures.

The profile scan resolves public actions and treats only admitted workflows as
passing:

```sh
mise run corpus:oss-profile
mise run corpus:oss-profile -- bat-changelog jq-valgrind
```

The profile applies the same `hosted-tokenless` admission policy as production,
but still does not execute action or repository code. Mutable action tags remain
as declared upstream, so these checks are observational: runtime comparisons
must separately retain immutable resolved action locks.

The Buildkite pipeline runs the full profile scan as one soft-failing step. It
publishes a build annotation listing each admitted or blocked case and its
diagnostic codes. The step remains visibly soft-failed without blocking the
pipeline until the corpus is fully admitted.

Actual Hosted execution must use a separately reviewed allowlist, an exact
event/source commit, disposable whole-job isolation, and no workflow secrets or
ambient provider credentials. This harness intentionally stops before that
authority boundary.

Both corpus commands are opt-in; the normal `mise run check` remains
network-free.
