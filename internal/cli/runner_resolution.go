package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

func suggestedRunnerTargets(ctx context.Context, reports []compiler.Report) (map[string]compiler.RunnerTarget, error) {
	endpoint := os.Getenv("BUILDKITE_AGENT_ENDPOINT")
	jobID := os.Getenv("BUILDKITE_JOB_ID")
	jobToken := os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN")
	if endpoint == "" || jobID == "" || jobToken == "" {
		return nil, nil
	}
	resolver, err := gharuntime.NewAgentRunnerResolver(gharuntime.AgentRunnerResolverConfig{
		Endpoint: endpoint,
		JobID:    jobID,
		JobToken: jobToken,
	})
	if err != nil {
		return nil, err
	}
	requirements := uniqueRunnerRequirements(reports)
	suggestions, err := resolver.Resolve(ctx, requirements)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]gharuntime.RunnerRequirement, len(requirements))
	for _, requirement := range requirements {
		byID[requirement.ID] = requirement
	}
	targets := make(map[string]compiler.RunnerTarget, len(suggestions))
	for _, suggestion := range suggestions {
		requirement := byID[suggestion.ID]
		if len(requirement.Labels) != 1 || !runnerQueuePattern.MatchString(suggestion.Queue) {
			return nil, fmt.Errorf("runner resolution response contains an invalid target")
		}
		platform, err := compiler.ParsePlatform(suggestion.Platform)
		if err != nil {
			return nil, fmt.Errorf("runner resolution response contains an invalid target: %w", err)
		}
		label := strings.ToLower(strings.TrimSpace(requirement.Labels[0]))
		target := compiler.RunnerTarget{Queue: suggestion.Queue, Platform: platform}
		if existing, ok := targets[label]; ok && existing != target {
			return nil, fmt.Errorf("runner resolution response contains conflicting targets")
		}
		targets[label] = target
	}
	return targets, nil
}

func uniqueRunnerRequirements(reports []compiler.Report) []gharuntime.RunnerRequirement {
	seen := make(map[string]bool)
	var requirements []gharuntime.RunnerRequirement
	for _, report := range reports {
		for _, job := range report.Jobs {
			if len(job.RunsOn) == 0 {
				continue
			}
			encoded, _ := json.Marshal(job.RunsOn)
			key := string(encoded)
			if seen[key] {
				continue
			}
			seen[key] = true
			requirements = append(requirements, gharuntime.RunnerRequirement{
				ID:     fmt.Sprintf("r%d", len(requirements)+1),
				Labels: append([]string(nil), job.RunsOn...),
			})
		}
	}
	return requirements
}
