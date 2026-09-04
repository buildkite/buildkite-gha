package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestParseBuildkiteRepository(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/acme/widgets", "https://github.com/acme/widgets.git",
		"git://github.com/acme/widgets.git", "ssh://git@github.com/acme/widgets.git",
		"git@github.com:acme/widgets.git",
	} {
		t.Run(raw, func(t *testing.T) {
			provider, owner, name, cloneURL, err := parseBuildkiteRepository(raw)
			if err != nil || provider != "github" || owner != "acme" || name != "widgets" || cloneURL != "https://github.com/acme/widgets.git" {
				t.Fatalf("parseBuildkiteRepository() = %q, %q, %q, %q, %v", provider, owner, name, cloneURL, err)
			}
		})
	}
	t.Run("Origin", func(t *testing.T) {
		raw := "https://origin.cursor.com/git/acme/widgets.git"
		provider, owner, name, cloneURL, err := parseBuildkiteRepository(raw)
		if err != nil || provider != "cursor-origin" || owner != "acme" || name != "widgets" || cloneURL != raw {
			t.Fatalf("parseBuildkiteRepository() = %q, %q, %q, %q, %v", provider, owner, name, cloneURL, err)
		}
	})
	for _, raw := range []string{"", "http://github.com/a/b", "https://user:pass@github.com/a/b", "https://github.example/a/b", "https://github.com/a", "https://github.com/a/b/extra", "https://github.com/a/..", "git@evil.example:a/b", "ssh://user@github.com/a/b"} {
		t.Run("reject "+raw, func(t *testing.T) {
			if _, _, _, _, err := parseBuildkiteRepository(raw); err == nil {
				t.Fatalf("parseBuildkiteRepository(%q) unexpectedly succeeded", raw)
			}
		})
	}
	for _, raw := range []string{
		"http://origin.cursor.com/git/acme/widgets.git",
		"ssh://git@origin.cursor.com/git/acme/widgets.git",
		"https://origin.cursor.com/acme/widgets.git",
		"https://origin.cursor.com/git/acme/widgets",
		"https://origin.cursor.com/git/acme/widgets.git/extra",
		"https://origin.cursor.com/git/acme/widgets.git?ref=main",
		"https://origin.cursor.com/git/acme/widgets.git?",
		"https://origin.cursor.com/git/acme/widgets.git#",
		"https://origin.cursor.com/git/acme/widgets%2Egit",
		"https://user:pass@origin.cursor.com/git/acme/widgets.git",
	} {
		t.Run("reject Origin "+raw, func(t *testing.T) {
			if _, _, _, _, err := parseBuildkiteRepository(raw); err == nil {
				t.Fatalf("parseBuildkiteRepository(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestBuildkiteEventSourceOriginIdentity(t *testing.T) {
	raw := "https://origin.cursor.com/git/acme/widgets.git"
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "importer",
		"BUILDKITE_REPO": raw, "BUILDKITE_COMMIT": strings.Repeat("a", 40),
		"BUILDKITE_BRANCH": "main", "BUILDKITE_BUILD_AUTHOR": "Origin Author",
	}
	source, err := buildkiteEventSource(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	repository := snapshot["repository"].(map[string]any)
	if snapshot["provider"] != "cursor-origin" || snapshot["event"] != "push" ||
		repository["owner"] != "acme" || repository["name"] != "widgets" || repository["clone_url"] != raw {
		t.Fatalf("Origin snapshot = %#v", snapshot)
	}
}

func TestBuildkiteWebhookEventSourceOriginGitHubCompatibleMetadata(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "importer",
		"BUILDKITE_REPO":         "https://origin.cursor.com/git/acme/widgets.git",
		"BUILDKITE_COMMIT":       strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":       "feature",
		"BUILDKITE_GITHUB_EVENT": "pull_request",
		"BUILDKITE_BUILD_AUTHOR": "Origin Author",
	}
	webhook := []byte(`{"action":"opened","repository":{"full_name":"untrusted/other"},"sender":{"login":"origin-user"}}`)
	source, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, webhook)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	repository := snapshot["repository"].(map[string]any)
	payload := snapshot["payload"].(map[string]any)
	if snapshot["provider"] != "cursor-origin" || snapshot["event"] != "pull_request" || snapshot["actor"] != "origin-user" ||
		repository["owner"] != "acme" || repository["name"] != "widgets" || payload["action"] != "opened" {
		t.Fatalf("Origin webhook snapshot = %#v", snapshot)
	}
}

func TestBuildkiteEventSourcePrefersGitHubEventName(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "importer",
		"BUILDKITE_REPO":         "https://github.com/acme/widgets.git",
		"BUILDKITE_COMMIT":       strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":       "main",
		"BUILDKITE_PULL_REQUEST": "false",
		"BUILDKITE_GITHUB_EVENT": "pull_request",
		"GITHUB_EVENT_NAME":      "push",
	}
	source, err := buildkiteEventSource(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["event"] != "push" || snapshot["ref"] != "refs/heads/main" {
		t.Fatalf("snapshot = %#v, want preferred GITHUB_EVENT_NAME push identity", snapshot)
	}

	env["GITHUB_EVENT_NAME"] = " push"
	if _, err := buildkiteEventSource(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "GITHUB_EVENT_NAME") {
		t.Fatalf("buildkiteEventSource() error = %v, want malformed preferred event failure", err)
	}
}

func TestBuildkiteEventSourceMappings(t *testing.T) {
	base := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "importer",
		"BUILDKITE_REPO":         "git@github.com:acme/widgets.git",
		"BUILDKITE_COMMIT":       "0123456789abcdef0123456789abcdef01234567",
		"BUILDKITE_BUILD_AUTHOR": "Unverified Author",
	}
	tests := []struct {
		name, event, ref string
		env              map[string]string
		check            func(t *testing.T, snapshot map[string]any)
	}{
		{name: "branch without preferred workflow ref", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main"}},
		{name: "preferred workflow branch ref", event: "push", ref: "refs/heads/triggered", env: map[string]string{"BUILDKITE_BRANCH": "built", githubEventNameEnvironment: "push", githubWorkflowRefEnvironment: "acme/widgets/.github/workflows/ci.yml@refs/heads/triggered"}},
		{name: "UI", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "ui"}},
		{name: "API", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "api"}},
		{name: "schedule", event: "schedule", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "schedule"}},
		{name: "empty source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": ""}},
		{name: "unknown source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "unknown"}},
		{name: "webhook source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "webhook"}},
		{name: "trigger job source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "trigger_job"}},
		{name: "rebuilt push preserves GitHub event", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "ui", "BUILDKITE_GITHUB_EVENT": "push"}},
		{name: "authoritative dispatch overrides UI fallback", event: "workflow_dispatch", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "ui", "BUILDKITE_GITHUB_EVENT": "workflow_dispatch"}},
		{name: "tag without preferred workflow ref", event: "push", ref: "refs/tags/v1.2.3", env: map[string]string{"BUILDKITE_TAG": "v1.2.3"}},
		{name: "preferred workflow tag ref", event: "push", ref: "refs/tags/v2.0.0", env: map[string]string{"BUILDKITE_TAG": "built-tag", githubEventNameEnvironment: "push", githubWorkflowRefEnvironment: "acme/widgets/.github/workflows/release.yml@refs/tags/v2.0.0"}},
		{name: "pull request without preferred workflow ref", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "contributor:feature", "BUILDKITE_PULL_REQUEST_BASE_BRANCH": "main", "BUILDKITE_PULL_REQUEST_REPO": "https://github.com/contributor/widgets.git"}, check: func(t *testing.T, snapshot map[string]any) {
			t.Helper()
			payload := snapshot["payload"].(map[string]any)
			pr := payload["pull_request"].(map[string]any)
			head := pr["head"].(map[string]any)
			headRepo := head["repo"].(map[string]any)
			baseRepo := pr["base"].(map[string]any)["repo"].(map[string]any)
			if payload["action"] != "synchronize" || payload["number"] != float64(42) || head["sha"] != base["BUILDKITE_COMMIT"] || head["ref"] != "feature" || headRepo["full_name"] != "contributor/widgets" || baseRepo["full_name"] != "acme/widgets" || pr["base"].(map[string]any)["ref"] != "main" {
				t.Fatalf("pull request payload = %#v", payload)
			}
		}},
		{name: "Pipeline Trigger preferred pull request ref and action", event: "pull_request", ref: "refs/pull/42/merge", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "feature", "BUILDKITE_GITHUB_ACTION": "opened", "BUILDKITE_GITHUB_WORKFLOW_PATH": ".github/workflows/ci.yml", githubEventNameEnvironment: "pull_request", githubWorkflowRefEnvironment: "acme/widgets/.github/workflows/ci.yml@refs/pull/42/merge"}, check: func(t *testing.T, snapshot map[string]any) {
			t.Helper()
			if action := snapshot["payload"].(map[string]any)["action"]; action != "opened" {
				t.Fatalf("pull request action = %#v, want opened", action)
			}
		}},
		{name: "pull request overrides UI", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "feature", "BUILDKITE_SOURCE": "ui"}},
		{name: "pull request overrides API", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "feature", "BUILDKITE_SOURCE": "api"}},
		{name: "pull request overrides schedule", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "feature", "BUILDKITE_SOURCE": "schedule"}},
		{name: "pull request overrides fallback source", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "feature", "BUILDKITE_SOURCE": "webhook"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{}
			maps.Copy(env, base)
			maps.Copy(env, test.env)
			source, err := buildkiteEventSource(func(key string) string { return env[key] })
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err := json.Unmarshal(source, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot["event"] != test.event || snapshot["ref"] != test.ref || snapshot["actor"] != "Unverified Author" {
				t.Fatalf("snapshot = %#v", snapshot)
			}
			if test.check != nil {
				test.check(t, snapshot)
			}
		})
	}
}

func TestGeneratedPullRequestEventIncludesRuntimeRefs(t *testing.T) {
	source, err := generatedEventSnapshot("pull_request")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Ref     string         `json:"ref"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	pullRequest := snapshot.Payload["pull_request"].(map[string]any)
	if snapshot.Ref != "refs/pull/1/merge" || pullRequest["base"].(map[string]any)["ref"] != "main" || pullRequest["head"].(map[string]any)["ref"] != "example" {
		t.Fatalf("generated pull request event = %#v", snapshot)
	}
}

func TestGeneratedReleaseEventIsCanonicalPublishedStableRelease(t *testing.T) {
	source, err := generatedEventSnapshot("release")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Event   string         `json:"event"`
		Ref     string         `json:"ref"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	release := snapshot.Payload["release"].(map[string]any)
	if snapshot.Event != "release" || snapshot.Ref != "refs/tags/v1.0.0" || snapshot.Payload["action"] != "published" || release["tag_name"] != "v1.0.0" || release["draft"] != false || release["prerelease"] != false {
		t.Fatalf("generated release event = %#v", snapshot)
	}
}

func TestGeneratedIssuesEventIsCanonicalOpenedIssue(t *testing.T) {
	source, err := generatedEventSnapshot("issues")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Event   string         `json:"event"`
		Ref     string         `json:"ref"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Payload["issue"].(map[string]any)
	if snapshot.Event != "issues" || snapshot.Ref != "refs/heads/main" || snapshot.Payload["action"] != "opened" || issue["number"] != float64(1) {
		t.Fatalf("generated issues event = %#v", snapshot)
	}
}

func TestBuildkiteEventSourceFailsClosed(t *testing.T) {
	valid := map[string]string{"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step", "BUILDKITE_REPO": "https://github.com/a/b", "BUILDKITE_COMMIT": strings.Repeat("a", 40), "BUILDKITE_BRANCH": "main"}
	for _, test := range []struct{ name, key, value string }{
		{"not Buildkite", "BUILDKITE", "false"},
		{"bad repo", "BUILDKITE_REPO", "https://example.com/a/b"}, {"symbolic commit", "BUILDKITE_COMMIT", "HEAD"},
		{"uppercase commit", "BUILDKITE_COMMIT", strings.Repeat("A", 40)},
		{"missing branch", "BUILDKITE_BRANCH", ""}, {"malformed PR", "BUILDKITE_PULL_REQUEST", "nope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{}
			maps.Copy(env, valid)
			env[test.key] = test.value
			if _, err := buildkiteEventSource(func(key string) string { return env[key] }); err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
	env := map[string]string{}
	maps.Copy(env, valid)
	env["BUILDKITE_PULL_REQUEST"], env["BUILDKITE_TAG"] = "3", "v1"
	if _, err := buildkiteEventSource(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("error = %v", err)
	}
}

func TestBuildkiteWebhookEventSourceUsesPayloadButPreservesExecutionIdentity(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO":         "https://github.com/buildkite/buildkite-gha",
		"BUILDKITE_COMMIT":       strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":       "executed-branch",
		"BUILDKITE_BUILD_AUTHOR": "Build Author",
		"BUILDKITE_GITHUB_EVENT": "pull_request_target",
	}
	webhook := []byte("{\"ref\":\"refs/heads/trigger-branch\",\"after\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"repository\":{\"full_name\":\"other/trigger\"},\"sender\":{\"login\":\"octocat\"},\"nested\":{\"complete\":true}}")
	source, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, webhook)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	payload := snapshot["payload"].(map[string]any)
	repository := snapshot["repository"].(map[string]any)
	if snapshot["event"] != "pull_request_target" || snapshot["actor"] != "octocat" ||
		snapshot["sha"] != strings.Repeat("a", 40) || snapshot["ref"] != "refs/heads/executed-branch" ||
		repository["owner"] != "buildkite" || repository["name"] != "buildkite-gha" ||
		payload["after"] != strings.Repeat("b", 40) || payload["nested"].(map[string]any)["complete"] != true {
		t.Fatalf("webhook snapshot = %#v", snapshot)
	}
}

func TestBuildkiteWebhookEventSourceBindsMergeGroupIdentity(t *testing.T) {
	headSHA, baseSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	branch := "gh-readonly-queue/main/pr-1-deadbeef"
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO":                    "https://github.com/acme/widgets",
		"BUILDKITE_COMMIT":                  headSHA,
		"BUILDKITE_BRANCH":                  branch,
		"BUILDKITE_GITHUB_EVENT":            "merge_group",
		"BUILDKITE_MERGE_QUEUE_BASE_BRANCH": "main",
		"BUILDKITE_MERGE_QUEUE_BASE_COMMIT": baseSHA,
	}
	webhook := fmt.Sprintf(`{"action":"checks_requested","merge_group":{"head_ref":"refs/heads/%s","head_sha":"%s","base_ref":"refs/heads/main","base_sha":"%s"}}`, branch, headSHA, baseSHA)
	source, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, []byte(webhook))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["event"] != "merge_group" || snapshot["ref"] != "refs/heads/"+branch || snapshot["sha"] != headSHA {
		t.Fatalf("merge_group snapshot = %#v", snapshot)
	}

	for _, test := range []struct{ name, old, replacement, want string }{
		{name: "head", old: headSHA, replacement: strings.Repeat("c", 40), want: "head_sha"},
		{name: "base", old: baseSHA, replacement: strings.Repeat("c", 40), want: "base_sha"},
		{name: "action", old: "checks_requested", replacement: "destroyed", want: "action"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(webhook, test.old, test.replacement, 1)
			if _, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, []byte(changed)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("buildkiteWebhookEventSource() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildkiteWebhookEventSourceBindsReleaseIdentity(t *testing.T) {
	commit := strings.Repeat("a", 40)
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO":          "https://github.com/acme/widgets",
		"BUILDKITE_COMMIT":        commit,
		"BUILDKITE_BRANCH":        "v1.2.3",
		"BUILDKITE_TAG":           "v1.2.3",
		"BUILDKITE_GITHUB_EVENT":  "release",
		"BUILDKITE_GITHUB_ACTION": "published",
	}
	webhook := `{"action":"published","release":{"tag_name":"v1.2.3","draft":false,"prerelease":false},"sender":{"login":"octocat"},"extra":{"preserved":true}}`
	source, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, []byte(webhook))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	payload := snapshot["payload"].(map[string]any)
	if snapshot["event"] != "release" || snapshot["ref"] != "refs/tags/v1.2.3" || snapshot["sha"] != commit || snapshot["actor"] != "octocat" || payload["extra"].(map[string]any)["preserved"] != true {
		t.Fatalf("release snapshot = %#v", snapshot)
	}
	for _, activity := range []struct {
		action     string
		prerelease bool
	}{{"published", true}, {"released", false}, {"created", false}} {
		t.Run(activity.action, func(t *testing.T) {
			changed := maps.Clone(env)
			changed["BUILDKITE_GITHUB_ACTION"] = activity.action
			payload := fmt.Sprintf(`{"action":%q,"release":{"tag_name":"v1.2.3","draft":false,"prerelease":%t}}`, activity.action, activity.prerelease)
			if _, err := buildkiteWebhookEventSource(func(key string) string { return changed[key] }, []byte(payload)); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, test := range []struct {
		name      string
		changeEnv func(map[string]string)
		webhook   string
		want      string
	}{
		{name: "event mismatch", changeEnv: func(env map[string]string) { env["BUILDKITE_GITHUB_EVENT"] = "push" }, webhook: webhook, want: "BUILDKITE_GITHUB_EVENT"},
		{name: "non-GitHub repository", changeEnv: func(env map[string]string) { env["BUILDKITE_REPO"] = "https://origin.cursor.com/git/acme/widgets.git" }, webhook: webhook, want: "GitHub repository"},
		{name: "action mismatch", changeEnv: func(env map[string]string) { env["BUILDKITE_GITHUB_ACTION"] = "released" }, webhook: webhook, want: "BUILDKITE_GITHUB_ACTION"},
		{name: "unsupported action", changeEnv: func(env map[string]string) { env["BUILDKITE_GITHUB_ACTION"] = "edited" }, webhook: strings.Replace(webhook, "published", "edited", 1), want: "unsupported"},
		{name: "tag mismatch", changeEnv: func(env map[string]string) { env["BUILDKITE_TAG"] = "v2.0.0" }, webhook: webhook, want: "BUILDKITE_TAG"},
		{name: "branch mismatch", changeEnv: func(env map[string]string) { env["BUILDKITE_BRANCH"] = "main" }, webhook: webhook, want: "BUILDKITE_TAG"},
		{name: "missing release", webhook: `{"action":"published"}`, want: "payload.release"},
		{name: "missing tag", webhook: `{"action":"published","release":{"draft":false,"prerelease":false}}`, want: "tag_name"},
		{name: "malformed draft", webhook: `{"action":"published","release":{"tag_name":"v1.2.3","draft":"false","prerelease":false}}`, want: "draft"},
		{name: "malformed prerelease", webhook: `{"action":"published","release":{"tag_name":"v1.2.3","draft":false,"prerelease":"false"}}`, want: "prerelease"},
		{name: "draft created", changeEnv: func(env map[string]string) { env["BUILDKITE_GITHUB_ACTION"] = "created" }, webhook: `{"action":"created","release":{"tag_name":"v1.2.3","draft":true,"prerelease":false}}`, want: "draft created"},
		{name: "symbolic commit", changeEnv: func(env map[string]string) { env["BUILDKITE_COMMIT"] = "HEAD" }, webhook: webhook, want: "BUILDKITE_COMMIT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := maps.Clone(env)
			if test.changeEnv != nil {
				test.changeEnv(changed)
			}
			_, err := buildkiteWebhookEventSource(func(key string) string { return changed[key] }, []byte(test.webhook))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("buildkiteWebhookEventSource() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildkiteEventSourceDoesNotInventReleaseFromEnvironment(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO":          "https://github.com/acme/widgets",
		"BUILDKITE_COMMIT":        strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":        "v1.2.3",
		"BUILDKITE_TAG":           "v1.2.3",
		"BUILDKITE_GITHUB_EVENT":  "release",
		"BUILDKITE_GITHUB_ACTION": "published",
	}
	source, err := buildkiteEventSource(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["event"] != "push" || snapshot["ref"] != "refs/tags/v1.2.3" {
		t.Fatalf("environment fallback snapshot = %#v", snapshot)
	}
}

func TestBuildkiteWebhookPushUsesBranchRefForPullRequestAssociatedBuild(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO":         "https://github.com/buildkite/buildkite-gha",
		"BUILDKITE_COMMIT":       strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":       "feature",
		"BUILDKITE_PULL_REQUEST": "42",
		"BUILDKITE_GITHUB_EVENT": "push",
	}
	source, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, []byte(`{"ref":"refs/heads/feature"}`))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["event"] != "push" || snapshot["ref"] != "refs/heads/feature" {
		t.Fatalf("snapshot event/ref = %q / %q", snapshot["event"], snapshot["ref"])
	}
}

func TestBuildkiteEventSourceRebuiltPushResetsPullRequestPayload(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO":         "https://github.com/buildkite/buildkite-gha",
		"BUILDKITE_COMMIT":       strings.Repeat("a", 40),
		"BUILDKITE_BRANCH":       "feature",
		"BUILDKITE_PULL_REQUEST": "42",
		"BUILDKITE_GITHUB_EVENT": "push",
		"BUILDKITE_SOURCE":       "ui",
	}
	source, err := buildkiteEventSource(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	payload := snapshot["payload"].(map[string]any)
	if snapshot["event"] != "push" || snapshot["ref"] != "refs/heads/feature" || payload["ref"] != "refs/heads/feature" || len(payload) != 1 {
		t.Fatalf("rebuilt push snapshot = %#v", snapshot)
	}
}

func TestBuildkiteWebhookEventSourceEventAndActorFallbacks(t *testing.T) {
	env := map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step",
		"BUILDKITE_REPO": "https://github.com/a/b", "BUILDKITE_COMMIT": strings.Repeat("a", 40),
		"BUILDKITE_BRANCH": "main", "BUILDKITE_BUILD_AUTHOR": "Build Author",
		"BUILDKITE_GITHUB_EVENT": "not valid",
	}
	source, err := buildkiteWebhookEventSource(func(key string) string { return env[key] }, []byte("{\"sender\":{\"login\":\" bad \"}}"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(source, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["event"] != "push" || snapshot["actor"] != "Build Author" {
		t.Fatalf("fallback snapshot = %#v", snapshot)
	}
}

func TestParseWebhookPayloadRejectsInvalidDocuments(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{name: "malformed", source: "{\"sender\":", want: "parse buildkite:webhook"},
		{name: "non-object", source: `[1,2]`, want: "must be a JSON object"},
		{name: "multiple", source: `{} {}`, want: "multiple JSON values"},
		{name: "conflicting duplicate", source: "{\"ref\":\"one\",\"ref\":\"two\"}", want: "conflicting duplicate object key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseWebhookPayload([]byte(test.source)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseWebhookPayload() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParseWebhookPayloadPreservesEmptyArrays(t *testing.T) {
	payload, err := parseWebhookPayload([]byte("{\"commits\":[],\"nested\":{\"requested_reviewers\":[]}}"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{\"commits\":[],\"nested\":{\"requested_reviewers\":[]}}" {
		t.Fatalf("encoded payload = %s", encoded)
	}
}
