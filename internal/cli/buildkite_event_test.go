package cli

import (
	"encoding/json"
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
	t.Run("Cursor Origin", func(t *testing.T) {
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
		t.Run("reject Cursor Origin "+raw, func(t *testing.T) {
			if _, _, _, _, err := parseBuildkiteRepository(raw); err == nil {
				t.Fatalf("parseBuildkiteRepository(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestBuildkiteEventSourceCursorOriginIdentity(t *testing.T) {
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
		t.Fatalf("Cursor Origin snapshot = %#v", snapshot)
	}
}

func TestBuildkiteWebhookEventSourceCursorOriginGitHubCompatibleMetadata(t *testing.T) {
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
		t.Fatalf("Cursor Origin webhook snapshot = %#v", snapshot)
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
		{name: "branch", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main"}},
		{name: "UI", event: "workflow_dispatch", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "ui"}},
		{name: "API", event: "workflow_dispatch", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "api"}},
		{name: "schedule", event: "schedule", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "schedule"}},
		{name: "empty source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": ""}},
		{name: "unknown source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "unknown"}},
		{name: "webhook source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "webhook"}},
		{name: "trigger job source fallback", event: "push", ref: "refs/heads/main", env: map[string]string{"BUILDKITE_BRANCH": "main", "BUILDKITE_SOURCE": "trigger_job"}},
		{name: "tag", event: "push", ref: "refs/tags/v1.2.3", env: map[string]string{"BUILDKITE_TAG": "v1.2.3"}},
		{name: "pull request head compatibility ref", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "contributor:feature", "BUILDKITE_PULL_REQUEST_BASE_BRANCH": "main", "BUILDKITE_PULL_REQUEST_REPO": "https://github.com/contributor/widgets.git"}, check: func(t *testing.T, snapshot map[string]any) {
			payload := snapshot["payload"].(map[string]any)
			pr := payload["pull_request"].(map[string]any)
			head := pr["head"].(map[string]any)
			headRepo := head["repo"].(map[string]any)
			baseRepo := pr["base"].(map[string]any)["repo"].(map[string]any)
			if payload["action"] != "synchronize" || payload["number"] != float64(42) || head["sha"] != base["BUILDKITE_COMMIT"] || head["ref"] != "feature" || headRepo["full_name"] != "contributor/widgets" || baseRepo["full_name"] != "acme/widgets" || pr["base"].(map[string]any)["ref"] != "main" {
				t.Fatalf("pull request payload = %#v", payload)
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
			for key, value := range base {
				env[key] = value
			}
			for key, value := range test.env {
				env[key] = value
			}
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

func TestBuildkiteEventSourceFailsClosed(t *testing.T) {
	valid := map[string]string{"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "step", "BUILDKITE_REPO": "https://github.com/a/b", "BUILDKITE_COMMIT": strings.Repeat("a", 40), "BUILDKITE_BRANCH": "main"}
	for _, test := range []struct{ name, key, value string }{
		{"not Buildkite", "BUILDKITE", "false"}, {"missing step", "BUILDKITE_STEP_KEY", ""},
		{"bad repo", "BUILDKITE_REPO", "https://example.com/a/b"}, {"symbolic commit", "BUILDKITE_COMMIT", "HEAD"},
		{"uppercase commit", "BUILDKITE_COMMIT", strings.Repeat("A", 40)},
		{"missing branch", "BUILDKITE_BRANCH", ""}, {"malformed PR", "BUILDKITE_PULL_REQUEST", "nope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{}
			for key, value := range valid {
				env[key] = value
			}
			env[test.key] = test.value
			if _, err := buildkiteEventSource(func(key string) string { return env[key] }); err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
	env := map[string]string{}
	for key, value := range valid {
		env[key] = value
	}
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
