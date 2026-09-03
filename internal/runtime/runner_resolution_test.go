package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentRunnerResolverBatchesRequirementsIgnoresUnknownFieldsAndReturnsSuggestions(t *testing.T) {
	const jobID = "11111111-1111-4111-8111-111111111111"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v3/jobs/"+jobID+"/github-actions/runners" || r.Header.Get("Authorization") != "Token job-token" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("User-Agent") != "buildkite-gha/1.2.3" {
			t.Errorf("request = %s %s, headers %#v", r.Method, r.URL.Path, r.Header)
		}
		var body struct {
			Requirements []struct {
				ID       string `json:"id"`
				Selector struct {
					Labels []string `json:"labels"`
				} `json:"selector"`
			} `json:"requirements"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		resolutions := make([]map[string]any, len(body.Requirements))
		for i, requirement := range body.Requirements {
			if len(requirement.Selector.Labels) == 1 && requirement.Selector.Labels[0] == "ubuntu-18.04" {
				resolutions[i] = map[string]any{
					"id":       requirement.ID,
					"target":   map[string]string{"queue": "linux-medium", "platform": "linux/amd64", "image": "registry.example.com/ubuntu@sha256:" + strings.Repeat("0", 64), "future": "ignored"},
					"warnings": []map[string]string{{"code": "runner_label_fallback", "message": "Runner labels [ubuntu-18.04] are not supported directly; using the linux-medium queue via a heuristic fallback. Configure an explicit runner mapping to use an appropriate Buildkite queue and avoid this fallback.", "future": "ignored"}},
					"future":   "ignored",
				}
			} else {
				resolutions[i] = map[string]any{"id": requirement.ID, "error": map[string]string{"code": "unmapped_labels", "message": "No compatible runner is configured.", "future": "ignored"}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resolutions": resolutions, "future": "ignored"})
	}))
	defer server.Close()

	resolver, err := NewAgentRunnerResolver(AgentRunnerResolverConfig{Endpoint: server.URL + "/v3", JobID: jobID, JobToken: "job-token", ClientVersion: "1.2.3", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	requirements := make([]RunnerRequirement, 101)
	for i := range requirements {
		requirements[i] = RunnerRequirement{ID: fmt.Sprintf("r%d", i), Labels: []string{"ubuntu-latest"}}
	}
	requirements[100].Labels = []string{"ubuntu-18.04"}
	suggestions, rejections, err := resolver.Resolve(t.Context(), requirements)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(suggestions) != 1 || suggestions[0].ID != "r100" || suggestions[0].Queue != "linux-medium" || suggestions[0].Platform != "linux/amd64" || suggestions[0].Image != "registry.example.com/ubuntu@sha256:"+strings.Repeat("0", 64) || len(suggestions[0].Warnings) != 1 || suggestions[0].Warnings[0] != (RunnerWarning{Code: "runner_label_fallback", Message: "Runner labels [ubuntu-18.04] are not supported directly; using the linux-medium queue via a heuristic fallback. Configure an explicit runner mapping to use an appropriate Buildkite queue and avoid this fallback."}) {
		t.Fatalf("requests/suggestions = %d / %#v", requests, suggestions)
	}
	if len(rejections) != 100 || rejections[0] != (RunnerRejection{ID: "r0", Code: "unmapped_labels", Message: "No compatible runner is configured."}) || rejections[99].ID != "r99" {
		t.Fatalf("rejections = %d / %#v", len(rejections), rejections[:min(2, len(rejections))])
	}
}

func TestAgentRunnerResolverRetainsServerRejectionsAndRejectsMalformedErrors(t *testing.T) {
	const jobID = "11111111-1111-4111-8111-111111111111"
	tests := []struct {
		name    string
		error   map[string]any
		want    RunnerRejection
		wantErr string
	}{
		{
			name:  "missing queue with unknown fields",
			error: map[string]any{"code": "missing_queue", "message": "Cluster 'Default' has no hosted macOS queue.", "cluster": "Default", "platform": "darwin/arm64", "required_queues": []string{"macos-medium"}},
			want:  RunnerRejection{ID: "r1", Code: "missing_queue", Message: "Cluster 'Default' has no hosted macOS queue."},
		},
		{
			name:  "unknown future code",
			error: map[string]any{"code": "queue_platform_mismatch", "message": "Queue targets Linux."},
			want:  RunnerRejection{ID: "r1", Code: "queue_platform_mismatch", Message: "Queue targets Linux."},
		},
		{name: "blank code", error: map[string]any{"code": " ", "message": "x"}, wantErr: "invalid error"},
		{name: "missing message", error: map[string]any{"code": "missing_queue"}, wantErr: "invalid error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"resolutions": []map[string]any{{"id": "r1", "error": test.error}}})
			}))
			defer server.Close()
			resolver, err := NewAgentRunnerResolver(AgentRunnerResolverConfig{Endpoint: server.URL + "/v3", JobID: jobID, JobToken: "job-token", ClientVersion: "1.2.3", Client: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			suggestions, rejections, err := resolver.Resolve(t.Context(), []RunnerRequirement{{ID: "r1", Labels: []string{"macos-latest"}}})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Resolve() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || len(suggestions) != 0 || len(rejections) != 1 || rejections[0] != test.want {
				t.Fatalf("Resolve() = %#v, %#v, %v", suggestions, rejections, err)
			}
		})
	}
}
