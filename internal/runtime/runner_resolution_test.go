package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentRunnerResolverBatchesRequirementsAndReturnsSuggestions(t *testing.T) {
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
			if len(requirement.Selector.Labels) == 1 && requirement.Selector.Labels[0] == "macos-26" {
				resolutions[i] = map[string]any{"id": requirement.ID, "target": map[string]string{"queue": "macos-26-medium", "platform": "darwin/arm64"}}
			} else {
				resolutions[i] = map[string]any{"id": requirement.ID, "error": map[string]string{"code": "unmapped_labels", "message": "No compatible runner is configured."}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resolutions": resolutions})
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
	requirements[100].Labels = []string{"macos-26"}
	suggestions, err := resolver.Resolve(t.Context(), requirements)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(suggestions) != 1 || suggestions[0] != (RunnerSuggestion{ID: "r100", Queue: "macos-26-medium", Platform: "darwin/arm64"}) {
		t.Fatalf("requests/suggestions = %d / %#v", requests, suggestions)
	}
}
