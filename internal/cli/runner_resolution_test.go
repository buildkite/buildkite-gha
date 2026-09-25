package cli

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
)

func TestSuggestedRunnerTargetsAgentEnvironment(t *testing.T) {
	for _, test := range []struct {
		name    string
		fields  map[string]any
		wantErr string
	}{
		{name: "native", fields: map[string]any{"agents": map[string]string{"nsc-gha-image": "ubuntu-24.04"}, "tool_cache": false}},
		{name: "legacy image", fields: map[string]any{"image": defaultNobleRunnerImage}},
		{name: "configured backend queue", fields: map[string]any{}},
		{name: "conflicting environment", fields: map[string]any{"image": defaultNobleRunnerImage, "agents": map[string]string{"nsc-gha-image": "ubuntu-24.04"}}, wantErr: "mutually exclusive"},
		{name: "queue override", fields: map[string]any{"agents": map[string]string{"queue": "other"}}, wantErr: "cannot override queue"},
		{name: "macos tags", fields: map[string]any{"platform": "darwin/arm64", "agents": map[string]string{"nsc-image-selectors": "macos.version=27.x"}}, wantErr: "only supported on linux"},
		{name: "windows tags", fields: map[string]any{"platform": "windows/amd64", "agents": map[string]string{"image": "windows"}}, wantErr: "only supported on linux"},
		{name: "empty tag", fields: map[string]any{"agents": map[string]string{"nsc-gha-image": ""}}, wantErr: "non-empty"},
		{name: "non-string tag", fields: map[string]any{"agents": map[string]any{"image": 42}}, wantErr: "decode runner"},
		{name: "non-boolean cache", fields: map[string]any{"tool_cache": "false"}, wantErr: "decode runner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := map[string]any{"queue": "linux-medium", "platform": "linux/amd64"}
			for key, value := range test.fields {
				target[key] = value
			}
			server, _ := runnerResolutionServer(t, http.StatusOK, map[string]map[string]any{"ubuntu-latest": {"target": target}})
			t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
			t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
			t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
			reports := []compiler.Report{{Jobs: []compiler.JobInstance{{RunsOn: []string{"ubuntu-latest"}}}}}
			got, err := suggestedRunnerTargets(t.Context(), reports, nil, "dev")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got.selectors) != 1 {
				t.Fatalf("selectors = %#v", got.selectors)
			}
			selected := got.selectors[0].Target
			if selected.Queue != "linux-medium" || selected.Platform != compiler.PlatformLinuxAMD64 {
				t.Fatalf("target = %#v", selected)
			}
			if tags, ok := test.fields["agents"]; ok && !reflect.DeepEqual(selected.Agents, tags) {
				t.Fatalf("agents = %#v", selected.Agents)
			}
			if cache, ok := test.fields["tool_cache"]; ok {
				if selected.ToolCache == nil || *selected.ToolCache != cache {
					t.Fatalf("tool cache = %v", selected.ToolCache)
				}
			} else if selected.ToolCache != nil {
				t.Fatalf("legacy tool cache = %v", selected.ToolCache)
			}
			if image, ok := test.fields["image"]; ok && selected.Image != image {
				t.Fatalf("image = %q", selected.Image)
			}
		})
	}
}

func TestExplicitRunnerImageBypassesNativeResolution(t *testing.T) {
	image := "registry.example.com/custom@sha256:" + strings.Repeat("a", 64)
	plugin, err := parsePluginConfiguration(`{"workflow":"workflow.yml","runners":[{"runs-on":"ubuntu-latest","queue":"custom","image":"` + image + `"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := parseUploadArgs([]string{"--runner-queue", "ubuntu-latest=custom", "--runner-image", "ubuntu-latest=" + image, "workflow.yml"})
	if err != nil {
		t.Fatal(err)
	}
	server, requests := runnerResolutionServer(t, http.StatusOK, map[string]map[string]any{"ubuntu-latest": {"target": map[string]any{"queue": "native", "platform": "linux/amd64", "agents": map[string]string{"nsc-gha-image": "ubuntu-24.04"}, "tool_cache": false}}})
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL+"/v3")
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	for _, targets := range []map[string]compiler.RunnerTarget{plugin.runnerTargets, cli.runnerTargets} {
		reports := []compiler.Report{{Jobs: []compiler.JobInstance{{RunsOn: []string{"ubuntu-latest"}}}}}
		resolution, err := suggestedRunnerTargets(t.Context(), reports, targets, "dev")
		if err != nil {
			t.Fatal(err)
		}
		options := hostedOptions("", targets, nil)
		applyRunnerResolution(&options, resolution)
		selected, err := options.Runners.Resolve([]string{"ubuntu-latest"}, compiler.EventUntrusted)
		if err != nil || selected.Image != image || selected.Queue != "custom" || len(selected.Agents) != 0 || *requests != 0 {
			t.Fatalf("selected=%#v requests=%d error=%v", selected, *requests, err)
		}
	}
}

func TestRunnerSelectorIsConfigured(t *testing.T) {
	linux := compiler.RunnerTarget{Queue: "linux", Platform: compiler.PlatformLinuxAMD64}
	cachedLinux := compiler.RunnerTarget{Queue: "linux", Platform: compiler.PlatformLinuxAMD64, Cache: &compiler.CacheVolume{Paths: []string{"/home/runner/.cache"}, Name: "dependencies", Size: "40g"}}
	equivalentCachedLinux := compiler.RunnerTarget{Queue: "linux", Platform: compiler.PlatformLinuxAMD64, Cache: &compiler.CacheVolume{Paths: []string{"/home/runner/.cache"}, Name: "dependencies", Size: "40g"}}
	other := compiler.RunnerTarget{Queue: "other", Platform: compiler.PlatformLinuxAMD64}
	targets := map[string]compiler.RunnerTarget{"ubuntu-18.04": linux, "self-hosted": linux, "cached": cachedLinux, "equivalent-cached": equivalentCachedLinux, "other": other}
	for _, test := range []struct {
		labels []string
		want   bool
	}{
		{labels: []string{"Ubuntu-18.04"}, want: true},
		{labels: []string{"self-hosted", "ubuntu-18.04"}, want: true},
		{labels: []string{"cached", "equivalent-cached"}, want: true},
		{labels: []string{"self-hosted", "missing"}},
		{labels: []string{"self-hosted", "other"}},
		{labels: nil},
	} {
		if got := runnerSelectorIsConfigured(test.labels, targets); got != test.want {
			t.Errorf("runnerSelectorIsConfigured(%q) = %v, want %v", test.labels, got, test.want)
		}
	}
}
