# Released-plugin demo fixtures

`.buildkite/plugin-demo.yml` runs the repository's example workflows through
the released `github-actions` plugin and CLI. The service-free examples run by
default. They include a native macOS arm64 proof for shell, JavaScript, and
composite steps. Set `DEMO_CACHE=1` to include the optional cache extension in
`.github/workflows/cache.yml`.
