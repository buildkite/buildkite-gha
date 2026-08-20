# Compatibility gap smoke tests

These fixtures continuously assess the 16 gaps in the linked [compatibility analysis](https://slopcannon.tail952194.ts.net/simone/gha-plugin-what-to-build-next/).

Run the assessment with:

```sh
mise run compatibility:gaps
```

The command applies the `hosted` profile to compile-time and admission gaps. It compiles and executes the two runtime shell fixtures. A passing case means the representative workflow is currently compatible. A blocked case makes the command fail.

CI soft-fails the aggregate step while gaps remain. After all cases finish, the step publishes one Buildkite annotation containing every result. This keeps the assessment visible without blocking unrelated builds.
