# Agent setup

This is a deepsec scanning workspace. Newly registered projects have a setup
prompt at `data/<id>/SETUP.md` — open it when asked to set that project up.

## Common tasks

- **Set up or resume a project**: run `npm exec -- deepsec setup`. For a
  scaffold-only/manual setup, read `data/<id>/SETUP.md` and follow it.
- **Add a new project**: run `npm exec -- deepsec init-project <root>` — it
  scaffolds `data/<id>/` and prints/writes the setup prompt for the
  new project.
- **Write a custom matcher** (only after a real true-positive shows you
  a pattern worth keeping): read
  `node_modules/deepsec/dist/docs/writing-matchers.md`.

## Reference

The deepsec skill is at `node_modules/deepsec/SKILL.md` (after `npm ci`). The
full docs ship at
`node_modules/deepsec/dist/docs/`.
