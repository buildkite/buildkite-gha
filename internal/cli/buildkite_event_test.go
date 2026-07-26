package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePublicGitHubRepository(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/acme/widgets", "https://github.com/acme/widgets.git",
		"git://github.com/acme/widgets.git", "ssh://git@github.com/acme/widgets.git",
		"git@github.com:acme/widgets.git",
	} {
		t.Run(raw, func(t *testing.T) {
			owner, name, cloneURL, err := parsePublicGitHubRepository(raw)
			if err != nil || owner != "acme" || name != "widgets" || cloneURL != "https://github.com/acme/widgets.git" {
				t.Fatalf("parsePublicGitHubRepository() = %q, %q, %q, %v", owner, name, cloneURL, err)
			}
		})
	}
	for _, raw := range []string{"", "http://github.com/a/b", "https://user:pass@github.com/a/b", "https://github.example/a/b", "https://github.com/a", "https://github.com/a/b/extra", "https://github.com/a/..", "git@evil.example:a/b", "ssh://user@github.com/a/b"} {
		t.Run("reject "+raw, func(t *testing.T) {
			if _, _, _, err := parsePublicGitHubRepository(raw); err == nil {
				t.Fatalf("parsePublicGitHubRepository(%q) unexpectedly succeeded", raw)
			}
		})
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
		{name: "tag", event: "push", ref: "refs/tags/v1.2.3", env: map[string]string{"BUILDKITE_TAG": "v1.2.3"}},
		{name: "pull request head compatibility ref", event: "pull_request", ref: "refs/pull/42/head", env: map[string]string{"BUILDKITE_PULL_REQUEST": "42", "BUILDKITE_BRANCH": "contributor:feature", "BUILDKITE_PULL_REQUEST_BASE_BRANCH": "main", "BUILDKITE_PULL_REQUEST_REPO": "https://github.com/contributor/widgets.git"}, check: func(t *testing.T, snapshot map[string]any) {
			payload := snapshot["payload"].(map[string]any)
			pr := payload["pull_request"].(map[string]any)
			head := pr["head"].(map[string]any)
			headRepo := head["repo"].(map[string]any)
			baseRepo := pr["base"].(map[string]any)["repo"].(map[string]any)
			if payload["number"] != float64(42) || head["sha"] != base["BUILDKITE_COMMIT"] || head["ref"] != "feature" || headRepo["full_name"] != "contributor/widgets" || baseRepo["full_name"] != "acme/widgets" || pr["base"].(map[string]any)["ref"] != "main" {
				t.Fatalf("pull request payload = %#v", payload)
			}
		}},
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
