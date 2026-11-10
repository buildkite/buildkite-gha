# deepsec

This directory holds the [deepsec](https://www.npmjs.com/package/deepsec)
configuration for the parent repository. It is checked into Git so teammates
inherit the reviewed threat model and custom matchers. Generated scan output is
ignored.

Currently configured project: `buildkite-gha` (target: `..`).

## Setup

Run the pinned CLI directly with the repository's Node 24 toolchain:

```bash
cd .deepsec
mise exec node@24 -- npx --yes deepsec@2.3.6 status
```

Local AI processing can reuse an authenticated Codex CLI. From the repository
root, run
`cd .deepsec && mise exec node@24 -- npx --yes --package deepsec@2.3.6 codex login status`
to check authentication with DeepSec's Codex CLI dependency. DeepSec also
supports model credentials through environment variables; never commit those
values.

## Daily commands

```bash
cd .deepsec
mise exec node@24 -- npx --yes deepsec@2.3.6 scan \
  --project-id buildkite-gha
mise exec node@24 -- npx --yes deepsec@2.3.6 process \
  --project-id buildkite-gha --agent codex --concurrency 4
mise exec node@24 -- npx --yes deepsec@2.3.6 revalidate \
  --project-id buildkite-gha --agent codex --min-severity MEDIUM
mise exec node@24 -- npx --yes deepsec@2.3.6 export \
  --project-id buildkite-gha --format md-dir --out ./findings
```

`scan` is local pattern matching. `process` and `revalidate` use the configured
AI agent and may incur model costs. Run state goes to `data/buildkite-gha/` and
is ignored; persist it as a private CI artifact only when incremental scans
need to resume across machines.

## Adding another project

To scan another codebase from this same `.deepsec/`:

```bash
cd .deepsec
mise exec node@24 -- npx --yes deepsec@2.3.6 init-project ../some-other-package
```

Appends an entry to `deepsec.config.ts` and writes
`data/<id>/{INFO.md,SETUP.md,project.json}`. Open the new SETUP.md
in your agent to fill in INFO.md.

## Layout

```
deepsec.config.ts        Project list (one entry per scanned repo)
data/buildkite-gha/
  INFO.md                Repo context — checked into git, hand-curated
  project.json           Generated (gitignored)
  tech.json              Generated (gitignored)
  files/                 One JSON per scanned source file (gitignored)
  runs/                  Run metadata (gitignored)
  revalidation/          Raw revalidation evidence (gitignored)
  reports/               Generated markdown reports (gitignored)
AGENTS.md                Pointer for coding agents
.env.local               Tokens (gitignored)
```

## Docs

Browse the DeepSec documentation on
[GitHub](https://github.com/vercel-labs/deepsec/tree/main/docs).
