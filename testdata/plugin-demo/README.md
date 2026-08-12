# Released-plugin demo fixtures

`.buildkite/plugin-demo.yml` runs the repository's example workflows through
the released `github-actions` plugin. The service-free examples run by default
and resolve the released CLI. The native macOS arm64 proof pins its released
runtime and covers shell, JavaScript, and composite steps. Set `DEMO_CACHE=1`
to include the optional cache extension in `.github/workflows/cache.yml`.
