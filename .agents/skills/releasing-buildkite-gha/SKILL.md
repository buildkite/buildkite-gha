---
name: releasing-buildkite-gha
description: Releases buildkite-gha and writes its release notes. Use for any buildkite-gha release, version proposal, release-note draft, publication, or post-release verification.
---

# Release buildkite-gha

Prepare and publish a release from the complete change set, with explicit approval and end-to-end verification.

## Preserve release history

- Treat every existing release tag as immutable. Never delete, move, force-push, recreate, or reuse one.
- Do not replace an existing release or its assets. Diagnose a failed publication and resume it only if the same tag still points to the intended commit and the repository's release process supports resuming safely.
- Stop before making changes if the intended tag or release already exists unexpectedly. Fetch current state, choose the next legal version from the latest stable tag when later work warrants another release, and obtain fresh approval.
- Do not publish, push a tag, create or edit a GitHub release, rerun a release build, or make another external write until the user explicitly approves that action.

## Inspect the release delta

1. Read `AGENTS.md`, `docs/development.md`, `mise.toml`, `scripts/release`, `.goreleaser.yml`, and the release job in `.buildkite/pipeline.yml` before relying on repository behavior.
2. Fetch the source of truth:

   ```sh
   if test "$(git rev-parse --is-shallow-repository)" = true; then
     git fetch --unshallow origin
   fi
   git fetch origin \
     +refs/heads/main:refs/remotes/origin/main --tags --prune
   ```

3. Confirm `origin/main` is the intended release commit. Record its full SHA. Do not release unpushed local work or a commit other than current `origin/main`.
4. Find the highest stable pre-1.0 tag reachable from `origin/main`. Ignore prereleases and tags not merged into `origin/main`:

   ```sh
   git tag --merged origin/main --list 'v0.*' --sort=-version:refname
   ```

   Select the first tag matching exactly `v0.<minor>.<patch>`, then require that tag to exist on `origin` and resolve to the same commit locally. Stop and reconcile the repository state if either check fails; never derive a version from a local-only or mismatched tag.

   ```sh
   local_previous=$(git rev-parse "$previous^{commit}")
   remote_previous=$(git ls-remote --exit-code origin \
     "refs/tags/$previous" "refs/tags/$previous^{}" |
     awk '$2 ~ /\^\{\}$/ { peeled=$1 } $2 !~ /\^\{\}$/ { tag=$1 }
       END { print peeled != "" ? peeled : tag }')
   test -n "$remote_previous"
   test "$local_previous" = "$remote_previous"
   ```
5. Inspect every commit and the complete aggregate diff from that tag through `origin/main`. Do not rely on merge subjects, conventional-commit prefixes, a generated changelog, or a diff summary alone.

   ```sh
   previous=v0.X.Y
   git log --reverse --format=fuller "$previous..origin/main"
   git rev-list --reverse "$previous..origin/main" |
     while read -r commit; do git show --stat --summary --format=fuller "$commit"; done
   git diff --stat "$previous..origin/main"
   git diff --find-renames "$previous..origin/main"
   ```

   Read changed source, tests, documentation, schemas, and release configuration as needed to understand user-visible behavior, compatibility boundaries, migrations, and security impact. Resolve every commit in the range before proposing a release.
6. Stop if there are no changes or the delta does not warrant a release. State any uncertainty instead of guessing.

## Choose the pre-1.0 version

- Choose the next minor version, `v0.(minor+1).0`, for additive compatibility, features, or breaking changes. Pre-1.0 breaking changes also increment the minor version.
- Choose the next patch version, `v0.minor.(patch+1)`, only when the complete delta contains fixes and internal-only changes.
- Do not skip versions or derive the bump from commit-message prefixes.
- Explain the proposed version with the highest-impact reason from the diff. If patch versus minor remains ambiguous, stop and ask.
- Verify that neither the remote tag nor a GitHub release already exists:

  ```sh
  git ls-remote --exit-code --tags --refs origin "refs/tags/$next"
  gh release view "$next" --repo buildkite/buildkite-gha
  ```

  Both commands should report absence. If either succeeds, do not rewrite or reuse the version.

## Draft release notes

Write concise, factual notes for users, not a raw commit list. Cover material features, fixes, compatibility changes, migrations, and security behavior; omit internal CI and test churn unless it changes user outcomes.

Use this shape:

```markdown
## Highlights

- <Most important user-visible change, with a PR or documentation link when useful.>
- <Other material changes, grouped into a few readable bullets.>

## Upgrade notes

- <Required action, rollout ordering, or important compatibility boundary.>

**Full comparison:** [v0.X.Y...v0.A.B](https://github.com/buildkite/buildkite-gha/compare/v0.X.Y...v0.A.B)
```

Omit `## Upgrade notes` when users need no action and there is no important compatibility boundary. Link notable pull requests, issues, or documentation in the relevant bullets; do not append an exhaustive commit list. The full comparison link is always required and must span the adjacent stable releases.

Save the exact draft in a temporary notes file. Validate it before presenting it:

```sh
grep -qx '## Highlights' "$notes_file"
grep -Fq "https://github.com/buildkite/buildkite-gha/compare/$previous...$next" "$notes_file"
test "$(git rev-list --count "$previous..origin/main")" -gt 0
```

## Obtain approval

Present together:

1. the proposed version;
2. the full release commit SHA;
3. the highest-impact reason for the bump; and
4. the complete draft notes.

Ask for explicit approval to publish that version with those notes. Stop and wait. Approval of a draft, investigation, or earlier release does not authorize publication. Incorporate requested edits and show the complete revised proposal before asking again when the version or substance changes.

For a release-note-only task, return the draft and stop unless the user explicitly asks to edit a published release.

## Publish the approved release

After approval, start again from a clean, up-to-date local `main`. Re-fetch `origin/main` and tags, confirm the approved SHA and version are unchanged, and recheck that the tag and release do not exist. If anything changed, stop and obtain approval for a new proposal.

Run the repository task and type the exact approved tag at its confirmation prompt:

```sh
mise run release -- "$next"
```

The task runs the repository check gate, validates clean and current `main`, creates the tag, and pushes it. Do not reproduce these steps manually or bypass its safeguards.

Immediately create the approved GitHub draft for the newly pushed tag so GoReleaser's `use_existing_draft` setting publishes that body after the tag build passes:

```sh
gh release create "$next" \
  --repo buildkite/buildkite-gha \
  --verify-tag \
  --draft \
  --title "$next" \
  --notes-file "$notes_file"
```

Confirm it is a draft with the exact approved body. If a release already exists, inspect it and stop rather than overwriting it. The tag build reruns checks before its publish job; monitor it through completion. Do not manually accelerate, rebuild, or retry shared CI without separate approval.

## Verify publication

Do not report success until all checks below pass.

1. **Tag and target:** fetch tags, then confirm the local tag, remote tag, `origin/main`, and approved SHA resolve to the same commit. Confirm the GitHub release is stable, published, and targets that commit.
2. **Body:** fetch the release body with `gh release view --json body` and compare it byte-for-byte with the approved notes file. Confirm the Highlights heading and full comparison link remain present.
3. **CI and release build:** verify all required GitHub checks and the Buildkite tag build passed, including the `publish-release` job. Use `bk build list --pipeline buildkite/buildkite-gha --commit "$commit" --json` and `bk build view <number> --pipeline buildkite/buildkite-gha --json` when Buildkite CLI authentication is available. Soft-fail reporting jobs do not block the release, but identify them accurately.
4. **Assets:** download the release into a new temporary directory and require exactly these published assets:
   - `buildkite-gha_Linux_x86_64.tar.gz`
   - `buildkite-gha_Darwin_arm64.tar.gz`
   - `checksums.txt`
5. **Checksums and binaries:** verify `checksums.txt` with `sha256sum` or `shasum`, inspect both archive listings, extract the Linux archive, and confirm its binary reports the released version. On macOS arm64, also execute the Darwin binary when that environment is available; otherwise report that only its checksum and archive contents were verified.

Useful commands:

```sh
git fetch origin +refs/heads/main:refs/remotes/origin/main --tags
commit=$(git rev-parse "$next^{commit}")
test "$commit" = "$(git rev-parse origin/main)"
test "$commit" = "$(git ls-remote origin "refs/tags/$next" | awk '{print $1}')"

gh release view "$next" --repo buildkite/buildkite-gha \
  --json tagName,targetCommitish,name,isDraft,isPrerelease,body,publishedAt,url,assets

published_notes=$(mktemp)
gh release view "$next" --repo buildkite/buildkite-gha \
  --json body --jq .body >"$published_notes"
cmp "$notes_file" "$published_notes"

assets_dir=$(mktemp -d)
gh release download "$next" --repo buildkite/buildkite-gha --dir "$assets_dir"
expected_assets=$(printf '%s\n' \
  buildkite-gha_Darwin_arm64.tar.gz \
  buildkite-gha_Linux_x86_64.tar.gz \
  checksums.txt | sort)
actual_assets=$(for asset in "$assets_dir"/*; do basename "$asset"; done | sort)
test "$actual_assets" = "$expected_assets"
if command -v sha256sum >/dev/null; then
  (cd "$assets_dir" && sha256sum --check checksums.txt)
else
  (cd "$assets_dir" && shasum -a 256 --check checksums.txt)
fi
tar -tzf "$assets_dir/buildkite-gha_Linux_x86_64.tar.gz"
tar -tzf "$assets_dir/buildkite-gha_Darwin_arm64.tar.gz"
tar -xzf "$assets_dir/buildkite-gha_Linux_x86_64.tar.gz" \
  -C "$assets_dir" buildkite-gha
test "$("$assets_dir/buildkite-gha" --version)" = \
  "buildkite-gha ${next#v}"
```

Report the release URL, tag and commit, Buildkite build and decisive job states, exact body match, assets, checksum result, and binary version result. State any verification that could not run.

## Edit published notes

For an explicitly requested historical release-note edit, first record the release ID, tag, target, name, draft/prerelease state, timestamps, and every asset's ID, name, size, digest, content type, and timestamps. Draft the replacement body and obtain approval before editing. Change only the body, then verify it exactly and confirm all recorded tag, release, and asset metadata remains unchanged. Never alter the tag, commit, binaries, checksums, or assets as part of a notes edit.
