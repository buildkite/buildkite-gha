# Starter workflow compatibility corpus

This corpus measures `buildkite-gha` against representative CI templates from
GitHub's official [`actions/starter-workflows`](https://github.com/actions/starter-workflows)
repository. It complements owned smoke fixtures: those prove specific runtime
contracts, while this corpus provides a stable product-facing compatibility
baseline across common ecosystems and workflow shapes.

The manifest pins one upstream commit and the SHA-256 of every raw template.
The harness verifies that raw source before replacing the documented
`$default-branch` template variable with `main`; any unsupported starter
variable left in the installed workflow fails the harness. It downloads only
workflow YAML and does not clone or execute third-party repository code.

The selected templates cover Go, Node.js, Python, Ruby, Rust, Java/Maven, .NET,
a Linux/Windows CMake matrix, a Rails PostgreSQL service, and Swift on macOS.
Selection is based on representative language and platform behavior, not on
whether a template currently passes:

| Case | Compatibility shape |
| --- | --- |
| `go` | Linux checkout and Go setup. |
| `node` | Three-row Node.js matrix with npm caching. |
| `python-package` | Three-row Python matrix and multiline shell steps. |
| `ruby` | Pinned third-party setup action, permissions, and Bundler caching. |
| `rust` | Linux checkout plus native toolchain commands. |
| `maven` | Java setup/cache and dependency submission. |
| `dotnet` | .NET setup and restore/build/test sequence. |
| `cmake-multi-platform` | Linux/Windows include/exclude matrix, outputs, and platform-native tools. |
| `rubyonrails` | PostgreSQL service container and a second lint job. |
| `swift` | macOS runner and native Swift toolchain. |

The CMake case is disabled by default because its matrix requires a Windows
runner. Passing its case ID explicitly runs it.

Run the networked compile and production-profile scans with:

```sh
mise run corpus:starter
mise run corpus:starter-profile
mise run corpus:starter-profile -- go cmake-multi-platform swift
```

Each command prints every result, reports passing, blocked, and disabled cases,
and exits non-zero while any enabled template misses its target state. The
profile scan resolves public actions and applies the `hosted` admission policy,
but neither mode executes action or project code. Mutable action tags remain as
declared by the official template, so profile results are observational and
runtime proof must retain immutable resolved action locks.

Buildkite runs the profile scan as a soft-failing step and publishes the result
as an annotation. Both corpus commands are opt-in; `mise run check` remains
network-free.
