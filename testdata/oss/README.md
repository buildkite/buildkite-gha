# Open-source workflow compatibility corpus

This corpus measures unchanged workflows from public repositories against
`buildkite-gha`. It complements the owned smoke fixtures: those prove specific
contracts, while these cases reveal compatibility gaps in ordinary workflows
that were not written for this project.

Each manifest entry pins the source repository commit and workflow path. The
harness fetches that exact commit anonymously into a temporary directory; it
clears ambient Git credentials, configuration, proxies, hooks, filters, and
transport overrides, then verifies the checked-out workflow bytes against the
pinned Git blob. It does not vendor third-party source or execute repository
code. Action references remain exactly as the upstream workflow declared them,
including mutable tags. Profile checks against mutable tags are intentionally
observational: the harness asserts the resulting compatibility boundary, not
the resolved action commit. A tag movement that preserves that boundary is not
reported. Hosted runtime comparisons must separately retain the immutable
resolved action locks.

The initial ten cases deliberately mix outcomes:

| Case | Ecosystem/shape | Exact GitHub run | Current boundary |
| --- | --- | --- | --- |
| `urfave-cli-lint` | Go setup and third-party lint action | [Success](https://github.com/urfave/cli/actions/runs/29794452917) | Setup action's compound token default blocks profile evaluation. |
| `fastify-markdown-lint` | Small Node lint job with pinned actions | — | Setup action's compound token default blocks profile evaluation. |
| `prettier-lint` | Larger Yarn, cache, condition, and concurrency workflow | [Success](https://github.com/prettier/prettier/actions/runs/31316608109) | Setup action's compound token default blocks profile evaluation. |
| `bat-changelog` | Pull-request shell and output workflow | — | Admitted; runtime needs a faithful real PR event. |
| `p-map-main` | Two-entry Node matrix | [Success](https://github.com/sindresorhus/p-map/actions/runs/29779845179) | Setup action's compound token default blocks profile evaluation. |
| `fzf-linux` | Go, Ruby, multiple shells, and host package setup | [Success](https://github.com/junegunn/fzf/actions/runs/31253573599) | Setup action's compound token default blocks profile evaluation; checkout also requests full history. |
| `jq-valgrind` | C/autotools/Valgrind plus failure artifact | [Success](https://github.com/jqlang/jq/actions/runs/30182447884) | Artifact action commit is outside the audited adapter. |
| `go-task-ci` | Five-job Go matrix | [Success](https://github.com/go-task/task/actions/runs/31330279154) | Compound concurrency expression is unsupported. |
| `gum-build` | Remote reusable workflow | — | Remote reuse and runtime secret forwarding are unsupported. |
| `just-ci` | Rust and a mixed-OS matrix | [Success](https://github.com/casey/just/actions/runs/31140381397) | Non-Linux runner rows are unsupported. |

Run the networked compile corpus with:

```sh
mise run corpus:oss
```

This clones pinned repositories, validates every workflow, and compiles every
compatible workflow twice to prove deterministic output. It does not resolve
actions or execute workflow commands. Seven cases also retain a successful
upstream GitHub run at the exact source SHA as the future differential oracle.

Profile checks anonymously resolve public actions and consume GitHub's small
unauthenticated API allowance. They are therefore explicit and case-selective:

```sh
mise run corpus:oss-profile -- bat-changelog
mise run corpus:oss-profile -- fzf-linux jq-valgrind
```

The profile applies the same `hosted-tokenless` admission policy as production,
but still does not execute action or repository code. It verifies the expected
graph size and boundary-specific diagnostic or warning. A GitHub API rate-limit
response is reported as an environmental failure, never accepted as the case's
expected compatibility result.

The `runtime` field is a review gate, not runtime evidence:

- `candidate-after-admission` is small and credential-free enough to review for
  eventual execution once its admission blocker is removed;
- `deferred` needs event fidelity, heavier dependencies, or more review;
- `unsupported` deliberately records a current fail-closed boundary.

Actual Hosted execution must use a separately reviewed allowlist, an exact
event/source commit, disposable whole-job isolation, and no workflow secrets or
ambient provider credentials. This harness intentionally stops before that
authority boundary.
