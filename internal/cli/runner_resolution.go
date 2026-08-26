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

func suggestedRunnerTargets(ctx context.Context, reports []compiler.Report, configuredTargets map[string]compiler.RunnerTarget, clientVersion string) ([]compiler.RunnerSelector, []gharuntime.RunnerWarning, error) {
	endpoint := os.Getenv("BUILDKITE_AGENT_ENDPOINT")
	jobID := os.Getenv("BUILDKITE_JOB_ID")
	jobToken := os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN")
	if endpoint == "" || jobID == "" || jobToken == "" {
		return nil, nil, nil
	}
	resolver, err := gharuntime.NewAgentRunnerResolver(gharuntime.AgentRunnerResolverConfig{
		Endpoint:      endpoint,
		JobID:         jobID,
		JobToken:      jobToken,
		ClientVersion: clientVersion,
	})
	if err != nil {
		return nil, nil, err
	}
	requirements := uniqueRunnerRequirements(reports, configuredTargets)
	suggestions, err := resolver.Resolve(ctx, requirements)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]gharuntime.RunnerRequirement, len(requirements))
	for _, requirement := range requirements {
		byID[requirement.ID] = requirement
	}
	selectors := make([]compiler.RunnerSelector, 0, len(suggestions))
	var warnings []gharuntime.RunnerWarning
	for _, suggestion := range suggestions {
		requirement := byID[suggestion.ID]
		labels := make([]string, len(requirement.Labels))
		seenLabels := make(map[string]bool, len(requirement.Labels))
		validLabels := len(requirement.Labels) != 0
		for i, label := range requirement.Labels {
			labels[i] = strings.ToLower(strings.TrimSpace(label))
			if labels[i] == "" || seenLabels[labels[i]] {
				validLabels = false
			}
			seenLabels[labels[i]] = true
		}
		if !validLabels {
			continue
		}
		if !runnerQueuePattern.MatchString(suggestion.Queue) {
			return nil, nil, fmt.Errorf("runner resolution response contains an invalid target")
		}
		platform, err := compiler.ParsePlatform(suggestion.Platform)
		if err != nil {
			return nil, nil, fmt.Errorf("runner resolution response contains an invalid target: %w", err)
		}
		if platform == compiler.PlatformLinuxAMD64 && !runnerImagePattern.MatchString(suggestion.Image) {
			return nil, nil, fmt.Errorf("runner resolution response contains an invalid target image")
		}
		if platform == compiler.PlatformDarwinARM64 && suggestion.Image != "" {
			return nil, nil, fmt.Errorf("runner resolution response contains an invalid target image")
		}
		target := compiler.RunnerTarget{Queue: suggestion.Queue, Platform: platform, Image: suggestion.Image}
		selectors = append(selectors, compiler.RunnerSelector{Labels: labels, Target: target})
		warnings = append(warnings, suggestion.Warnings...)
	}
	return selectors, warnings, nil
}

func uniqueRunnerRequirements(reports []compiler.Report, configuredTargets map[string]compiler.RunnerTarget) []gharuntime.RunnerRequirement {
	seen := make(map[string]bool)
	var requirements []gharuntime.RunnerRequirement
	for _, report := range reports {
		for _, job := range report.Jobs {
			if len(job.RunsOn) == 0 || runnerSelectorIsConfigured(job.RunsOn, configuredTargets) {
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

func runnerSelectorIsConfigured(labels []string, configuredTargets map[string]compiler.RunnerTarget) bool {
	var target compiler.RunnerTarget
	for i, label := range labels {
		configured, ok := configuredTargets[strings.ToLower(strings.TrimSpace(label))]
		if !ok || (i != 0 && !compiler.RunnerTargetsEqual(configured, target)) {
			return false
		}
		target = configured
	}
	return len(labels) != 0
}
