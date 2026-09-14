package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

// agentRunnerResolution is the Agent API's verdict on every runs-on selector.
// Explicit targets require validation rather than falling back on API failure.
type agentRunnerResolution struct {
	selectors          []compiler.RunnerSelector
	rejections         []compiler.RunnerRejection
	warnings           []gharuntime.RunnerWarning
	validationRequired bool
}

func (r agentRunnerResolution) empty() bool {
	return len(r.selectors) == 0 && len(r.rejections) == 0
}

func suggestedRunnerTargets(ctx context.Context, reports []compiler.Report, configuredTargets map[string]compiler.RunnerTarget, clientVersion string) (agentRunnerResolution, error) {
	requirements := uniqueRunnerRequirements(reports, configuredTargets)
	resolution := agentRunnerResolution{validationRequired: slices.ContainsFunc(requirements, func(requirement gharuntime.RunnerRequirement) bool {
		return requirement.ConfiguredTarget != nil
	})}
	endpoint := os.Getenv("BUILDKITE_AGENT_ENDPOINT")
	jobID := os.Getenv("BUILDKITE_JOB_ID")
	jobToken := os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN")
	if endpoint == "" || jobID == "" || jobToken == "" {
		if resolution.validationRequired {
			return resolution, fmt.Errorf("explicit runner mappings require job-scoped Agent API credentials")
		}
		return resolution, nil
	}
	resolver, err := gharuntime.NewAgentRunnerResolver(gharuntime.AgentRunnerResolverConfig{
		Endpoint:      endpoint,
		JobID:         jobID,
		JobToken:      jobToken,
		ClientVersion: clientVersion,
	})
	if err != nil {
		return resolution, err
	}
	suggestions, rejections, err := resolver.Resolve(ctx, requirements)
	if err != nil {
		return resolution, err
	}
	byID := make(map[string]gharuntime.RunnerRequirement, len(requirements))
	for _, requirement := range requirements {
		byID[requirement.ID] = requirement
	}
	for _, suggestion := range suggestions {
		requirement := byID[suggestion.ID]
		labels, ok := normalizedRunnerLabels(requirement.Labels)
		if !ok {
			continue
		}
		if configured := requirement.ConfiguredTarget; configured != nil {
			if !suggestion.Validated || suggestion.Queue != configured.Queue || suggestion.Platform != configured.Platform {
				return agentRunnerResolution{validationRequired: true}, fmt.Errorf("agent API did not validate the configured runner target; explicit mappings require server support")
			}
			// The server validates routing, not the customer's image or cache.
			resolution.selectors = append(resolution.selectors, compiler.RunnerSelector{Labels: labels, Target: configuredTargets[labels[0]]})
			continue
		}
		if !runnerQueuePattern.MatchString(suggestion.Queue) {
			return agentRunnerResolution{validationRequired: resolution.validationRequired}, fmt.Errorf("runner resolution response contains an invalid target")
		}
		platform, err := compiler.ParsePlatform(suggestion.Platform)
		if err != nil {
			return agentRunnerResolution{validationRequired: resolution.validationRequired}, fmt.Errorf("runner resolution response contains an invalid target: %w", err)
		}
		if platform == compiler.PlatformLinuxAMD64 && !runnerImagePattern.MatchString(suggestion.Image) {
			return agentRunnerResolution{validationRequired: resolution.validationRequired}, fmt.Errorf("runner resolution response contains an invalid target image")
		}
		if platform == compiler.PlatformDarwinARM64 && suggestion.Image != "" {
			return agentRunnerResolution{validationRequired: resolution.validationRequired}, fmt.Errorf("runner resolution response contains an invalid target image")
		}
		target := compiler.RunnerTarget{Queue: suggestion.Queue, Platform: platform, Image: suggestion.Image}
		resolution.selectors = append(resolution.selectors, compiler.RunnerSelector{Labels: labels, Target: target})
		resolution.warnings = append(resolution.warnings, suggestion.Warnings...)
	}
	for _, rejection := range rejections {
		labels, ok := normalizedRunnerLabels(byID[rejection.ID].Labels)
		if !ok {
			continue
		}
		resolution.rejections = append(resolution.rejections, compiler.RunnerRejection{Labels: labels, Code: rejection.Code, Message: rejection.Message})
	}
	return resolution, nil
}

// normalizedRunnerLabels lowercases one selector and reports whether it is a
// well-formed non-empty set of distinct labels.
func normalizedRunnerLabels(raw []string) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	labels := make([]string, len(raw))
	seen := make(map[string]bool, len(raw))
	for i, label := range raw {
		labels[i] = strings.ToLower(strings.TrimSpace(label))
		if labels[i] == "" || seen[labels[i]] {
			return nil, false
		}
		seen[labels[i]] = true
	}
	return labels, true
}

func uniqueRunnerRequirements(reports []compiler.Report, configuredTargets map[string]compiler.RunnerTarget) []gharuntime.RunnerRequirement {
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
			requirement := gharuntime.RunnerRequirement{
				ID:     fmt.Sprintf("r%d", len(requirements)+1),
				Labels: append([]string(nil), job.RunsOn...),
			}
			if runnerSelectorIsConfigured(job.RunsOn, configuredTargets) {
				target := configuredTargets[strings.ToLower(strings.TrimSpace(job.RunsOn[0]))]
				requirement.ConfiguredTarget = &gharuntime.ConfiguredRunnerTarget{Queue: target.Queue, Platform: target.Platform.String()}
			}
			requirements = append(requirements, requirement)
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
