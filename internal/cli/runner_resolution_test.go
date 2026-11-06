package cli

import (
	"bytes"
	"encoding/json"
	"html"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

func TestRunnerQueueSetupDiagnostic(t *testing.T) {
	const setupURL = `https://buildkite.com/organizations/acme/clusters/cluster/queues/new?queue=macos-medium&note="<review>"`
	const message = "Cluster 'Default & <team>' has no hosted macOS queue. Review " + setupURL
	for _, test := range []struct {
		name, code, url string
		wantAction      bool
	}{
		{name: "missing queue", code: "missing_queue", url: setupURL, wantAction: true},
		{name: "http app", code: "missing_queue", url: "http://buildkite.localhost:3000/queues/new?queue=macos-medium", wantAction: true},
		{name: "older server and prose URL", code: "missing_queue"},
		{name: "incompatible labels", code: "incompatible_labels", url: setupURL},
		{name: "explicit queue not found", code: "queue_not_found", url: setupURL},
		{name: "unsupported selector", code: "unsupported_selector", url: setupURL},
		{name: "unmapped labels", code: "unmapped_labels", url: setupURL},
		{name: "no cluster", code: "no_cluster", url: setupURL},
		{name: "javascript", code: "missing_queue", url: "javascript:alert(1)"},
		{name: "data", code: "missing_queue", url: "data:text/html,<script>alert(1)</script>"},
		{name: "relative", code: "missing_queue", url: "/queues/new"},
		{name: "protocol relative", code: "missing_queue", url: "//buildkite.com/queues/new"},
		{name: "opaque", code: "missing_queue", url: "https:buildkite.com/queues/new"},
		{name: "no host", code: "missing_queue", url: "https:///queues/new"},
		{name: "credentials", code: "missing_queue", url: "https://user:password@buildkite.com/queues/new"},
		{name: "control", code: "missing_queue", url: "https://buildkite.com/queues/\nnew"},
		{name: "whitespace", code: "missing_queue", url: " https://buildkite.com/queues/new"},
		{name: "backslash", code: "missing_queue", url: `https://buildkite.com\@example.com/queues/new`},
		{name: "malformed escape", code: "missing_queue", url: "https://buildkite.com/%zz"},
	} {
		t.Run(test.name, func(t *testing.T) {
			serverError := map[string]any{"code": test.code, "message": message}
			if test.url != "" {
				serverError["setup_url"] = test.url
			}
			setRunnerResolutionVerdicts(t, map[string]map[string]any{"macos-latest": {"error": serverError}})
			resolution, err := suggestedRunnerTargets(t.Context(), []compiler.Report{{Jobs: []compiler.JobInstance{{RunsOn: []string{"macos-latest"}}}}}, nil, "dev")
			if err != nil {
				t.Fatal(err)
			}
			options := compiler.DefaultOptions()
			applyRunnerResolution(&options, resolution)
			compiled, err := compiler.ValidateWithOptions("ci.yml", []byte("on: push\njobs:\n  mac:\n    runs-on: macos-latest\n    steps: [{run: true}]\n"), options)
			if err == nil {
				t.Fatal("missing queue must still reject compilation")
			}
			report := compatibility.InitialProcessingReport("ci.yml", "", false, compiled, err)
			report.Finalize()
			if len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, message) {
				t.Fatalf("server cause lost: %+v", report.Diagnostics)
			}
			wantURL := ""
			if test.wantAction {
				wantURL = test.url
			}
			var encoded, plain bytes.Buffer
			if err := compatibility.WriteProcessing(&encoded, "json", report); err != nil {
				t.Fatal(err)
			}
			var decoded compatibility.ProcessingReport
			if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Diagnostics[0].SetupURL != wantURL || strings.Contains(encoded.String(), `"setup_url"`) != test.wantAction {
				t.Fatalf("JSON setup URL: %s", encoded.String())
			}
			if err := compatibility.WriteProcessing(&plain, "text", report); err != nil {
				t.Fatal(err)
			}
			logs, _ := processingLog(t.Context(), report, sourceLinkContext{}, "Workflow could not be run")
			_, annotation := processingAnnotation(t.Context(), report, sourceLinkContext{})
			for name, output := range map[string]string{"text": plain.String(), "logs": strings.Join(logs, "\n"), "annotation": annotation} {
				if strings.Contains(output, "Set up queue") != test.wantAction || strings.Contains(output, compatibility.QueueSetupGuidance) != test.wantAction {
					t.Fatalf("%s action: %s", name, output)
				}
			}
			if test.wantAction && (!strings.Contains(annotation, `<a href="`+html.EscapeString(test.url)+`">Set up queue</a>`) || !strings.Contains(plain.String(), "Set up queue: "+test.url) || !strings.Contains(strings.Join(logs, "\n"), "Set up queue: "+test.url)) {
				t.Fatalf("action URL not preserved or escaped: %s\n%s\n%v", annotation, plain.String(), logs)
			}
			if strings.Contains(annotation, "<team>") || strings.Contains(annotation, "<review>") {
				t.Fatalf("unescaped server content: %s", annotation)
			}
		})
	}
}

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
		{name: "macos tags", fields: map[string]any{"platform": "darwin/arm64", "agents": map[string]string{"nsc-image-selectors": "macos.version=15.x,macos.purpose=githubrunner"}, "tool_cache": false}},
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
			if selected.Queue != "linux-medium" || selected.Platform.String() != target["platform"] {
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

func TestExplicitRunnerImageIsPreservedAfterValidation(t *testing.T) {
	image := "registry.example.com/custom@sha256:" + strings.Repeat("a", 64)
	plugin, err := parsePluginConfiguration(`{"workflow":"workflow.yml","runners":[{"runs-on":"ubuntu-latest","queue":"custom","image":"` + image + `"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := parseUploadArgs([]string{"--runner-queue", "ubuntu-latest=custom", "--runner-image", "ubuntu-latest=" + image, "workflow.yml"})
	if err != nil {
		t.Fatal(err)
	}
	server, requests := runnerResolutionServer(t, http.StatusOK, map[string]map[string]any{"ubuntu-latest": {"validated": true, "target": map[string]any{"queue": "custom", "platform": "linux/amd64", "agents": map[string]string{"nsc-gha-image": "ubuntu-24.04"}, "tool_cache": false}}})
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
		if err != nil || selected.Image != image || selected.Queue != "custom" || len(selected.Agents) != 0 {
			t.Fatalf("selected=%#v requests=%d error=%v", selected, *requests, err)
		}
	}
	if *requests != 2 {
		t.Fatalf("validation requests = %d, want 2", *requests)
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

func TestRunnerRequirementsIncludeExplicitMultiLabelTargets(t *testing.T) {
	target := compiler.RunnerTarget{Queue: "custom-linux", Platform: compiler.PlatformLinuxAMD64}
	configured := map[string]compiler.RunnerTarget{"self-hosted": target, "ubuntu-latest": target}
	jobs := []compiler.JobInstance{
		{RunsOn: []string{"self-hosted", "Ubuntu-Latest"}},
		{RunsOn: []string{"macos-latest"}},
		{RunsOn: []string{"self-hosted", "Ubuntu-Latest"}},
	}
	requirements := uniqueRunnerRequirements([]compiler.Report{{Jobs: jobs}}, configured)
	want := []gharuntime.RunnerRequirement{
		{ID: "r1", Labels: []string{"self-hosted", "Ubuntu-Latest"}, ConfiguredTarget: &gharuntime.ConfiguredRunnerTarget{Queue: "custom-linux", Platform: "linux/amd64"}},
		{ID: "r2", Labels: []string{"macos-latest"}},
	}
	if !reflect.DeepEqual(requirements, want) {
		t.Fatalf("requirements = %#v, want %#v", requirements, want)
	}
}
