// Package gha compiles and executes GitHub Actions workflows on Buildkite.
// Import it as github.com/buildkite/buildkite-gha.
//
// The exported types and Client methods are the stable embedding API. Compatible
// releases preserve their source and documented behavior; breaking API changes
// require a minor release while the module is v0, and a major release at v1+.
// Plans, stage records, and pipeline bytes are opaque, version-bound transport
// artifacts, not stable schemas. Do not construct or modify them.
//
// Typed methods install no signal handlers and do not change the process cwd or
// environment. They use the current checkout, Buildkite job environment, and
// executable identity just like the CLI. Compile prepares a hosted upload, not
// the CLI's offline compile command: it can read Buildkite metadata, resolve
// actions over the network, call the job-scoped Agent API, and annotate import
// diagnostics. It never uploads artifacts or a pipeline. Upload performs those
// publications only after preparation. At least one workflow path is required;
// missing paths fail preparation, just like CLI upload operands. Supported
// workflow failures that can safely become failing steps are successful imports.
//
// Provide Backend and Credentials explicitly. AgentAPI is an optional HTTP
// RoundTripper; nil uses the standard HTTP transport, never an agent subprocess.
// Job API credentials and endpoint still come from the job environment. HTTP
// authentication, scope checks, timeout, and redirect rejection remain owned by
// this module. The caller must not mutate dependencies during an invocation.
//
// The embedding executable is the default runtime distribution. RunJob verifies
// its version and digest against the plan. Generated commands retain
// the CLI bootstrap protocol (upload, run-job, and private runtime helpers);
// the embedding executable must dispatch that protocol, for example via RunCLI.
// Native agent command prefixes and bootstrap changes are not provided here.
// RunJob may publish terminal results with a bounded context after cancellation.
package gha
