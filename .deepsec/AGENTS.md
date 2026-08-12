# Agent setup

This is a deepsec scanning workspace. Newly registered projects have a setup
prompt at `data/<id>/SETUP.md` — open it when asked to set that project up.

## Common tasks

- **Install the workspace**: from the repository root, run
  `cd .deepsec && mise exec node@24 -- npm ci`.
- **Set up or resume a project**: from the repository root, run
  `cd .deepsec && mise exec node@24 -- npm exec -- deepsec setup`. For a
  scaffold-only/manual setup, read `data/<id>/SETUP.md` and follow it.
- **Add a new project**: from the repository root, run
  `cd .deepsec && mise exec node@24 -- npm exec -- deepsec init-project <root>`.
  It scaffolds `data/<id>/` and prints or writes the setup prompt for the new
  project.
- **Write a custom matcher** (only after a real true-positive shows you
  a pattern worth keeping): read
  `node_modules/deepsec/dist/docs/writing-matchers.md`.

## Reference

The deepsec skill is at `node_modules/deepsec/SKILL.md` (after `npm ci`). The
full docs ship at
`node_modules/deepsec/dist/docs/`.
