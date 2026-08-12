# deepsec

This directory holds the [deepsec](https://www.npmjs.com/package/deepsec)
configuration for the parent repository. It is checked into Git so teammates
inherit the reviewed threat model and custom matchers. Generated scan output is
ignored.

Currently configured project: `buildkite-gha` (target: `..`).

## Setup

Install the pinned workspace dependency with the repository's Node 24
toolchain:

```bash
mise exec node@24 -- npm ci
```

Local AI processing can reuse an authenticated Codex CLI. Run
`codex login status` to check authentication. DeepSec also supports model
credentials through environment variables; never commit those values.

## Daily commands

```bash
mise exec node@24 -- npm exec -- deepsec scan \
  --project-id buildkite-gha
mise exec node@24 -- npm exec -- deepsec process \
  --project-id buildkite-gha --agent codex --concurrency 4
mise exec node@24 -- npm exec -- deepsec revalidate \
  --project-id buildkite-gha --agent codex --min-severity MEDIUM
mise exec node@24 -- npm exec -- deepsec export \
  --project-id buildkite-gha --format md-dir --out ./findings
```

`scan` is local pattern matching. `process` and `revalidate` use the configured
AI agent and may incur model costs. Run state goes to `data/buildkite-gha/` and
is ignored; persist it as a private CI artifact only when incremental scans
need to resume across machines.

## Adding another project

To scan another codebase from this same `.deepsec/`:

```bash
mise exec node@24 -- npm exec -- deepsec init-project ../some-other-package
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

After `npm ci`:

- Skill: `node_modules/deepsec/SKILL.md`
- Full docs: `node_modules/deepsec/dist/docs/{getting-started,configuration,models,writing-matchers,plugins,architecture,data-layout,vercel-setup,faq}.md`

Or browse on
[GitHub](https://github.com/vercel/deepsec/tree/main/docs).
