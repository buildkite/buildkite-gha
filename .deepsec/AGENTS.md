# Agent setup

This is a deepsec scanning workspace. Newly registered projects have a setup
prompt at `data/<id>/SETUP.md` — open it when asked to set that project up.

## Common tasks

- **Set up or resume a project**: from the repository root, run
  `cd .deepsec && mise exec node@24 -- npx --yes deepsec@2.3.6 setup`. For a
  scaffold-only/manual setup, read `data/<id>/SETUP.md` and follow it.
- **Add a new project**: from the repository root, run
  `cd .deepsec && mise exec node@24 -- npx --yes deepsec@2.3.6 init-project <root>`.
  It scaffolds `data/<id>/` and prints or writes the setup prompt for the new
  project.
- **Write a custom matcher** only after a real true-positive shows you a
  pattern worth keeping. See the upstream DeepSec documentation.

## Reference

Keep the configuration dependency-free so the pinned DeepSec CLI can run
directly through `npx` without a local npm manifest or installation.
