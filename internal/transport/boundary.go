package transport

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

// Runner is the only process boundary used by the Buildkite adapter.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error)
}

// Agent invokes public buildkite-agent commands. Tests use a capture Runner.
type Agent struct {
	Runner Runner
}

func (a Agent) UploadArtifact(ctx context.Context, path string) error {
	_, err := a.run(ctx, []string{"artifact", "upload", path}, nil)
	return err
}

func (a Agent) DownloadArtifact(ctx context.Context, path, destination, producerStep string) error {
	if !keyPattern.MatchString(producerStep) {
		return fmt.Errorf("invalid producer step key %q", producerStep)
	}
	_, err := a.run(ctx, []string{"artifact", "download", path, destination, "--step", producerStep}, nil)
	return err
}

func (a Agent) SetMetadata(ctx context.Context, key, value string) error {
	_, err := a.run(ctx, []string{"meta-data", "set", key, value}, nil)
	return err
}

func (a Agent) GetMetadata(ctx context.Context, key string) ([]byte, error) {
	return a.run(ctx, []string{"meta-data", "get", key}, nil)
}

func (a Agent) UploadPipeline(ctx context.Context, pipeline []byte) error {
	_, err := a.run(ctx, []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets", "--reject-parse-warnings"}, pipeline)
	return err
}

func (a Agent) run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	if a.Runner == nil {
		return nil, fmt.Errorf("buildkite agent runner is required")
	}
	return a.Runner.Run(ctx, "buildkite-agent", args, stdin)
}

// Upload executes the security-relevant ordering: immutable plan artifacts,
// signed expected marker, dynamic pipeline, then signed completed marker.
func Upload(ctx context.Context, agent Agent, plans []PlanArtifact, pipeline []byte, expected, completed MarkerValue) error {
	if err := validateMarkerPair(pipeline, plans, expected, completed); err != nil {
		return err
	}
	plans = append([]PlanArtifact(nil), plans...)
	sort.Slice(plans, func(i, j int) bool { return plans[i].StepKey < plans[j].StepKey })
	for _, plan := range plans {
		if err := agent.UploadArtifact(ctx, plan.Path()); err != nil {
			return fmt.Errorf("upload plan %q: %w", plan.StepKey, err)
		}
		if err := agent.UploadArtifact(ctx, plan.BindingPath()); err != nil {
			return fmt.Errorf("upload plan binding %q: %w", plan.StepKey, err)
		}
	}
	if err := agent.SetMetadata(ctx, expected.key, expected.value); err != nil {
		return fmt.Errorf("publish expected marker: %w", err)
	}
	if err := agent.UploadPipeline(ctx, pipeline); err != nil {
		return fmt.Errorf("upload pipeline: %w", err)
	}
	if err := agent.SetMetadata(ctx, completed.key, completed.value); err != nil {
		return fmt.Errorf("publish completed marker: %w", err)
	}
	return nil
}

func validateMarkerPair(pipeline []byte, plans []PlanArtifact, expected, completed MarkerValue) error {
	digest := Digest(pipeline)
	planJobs := make([]UploadJob, 0, len(plans))
	for _, plan := range plans {
		if err := plan.validate(); err != nil {
			return err
		}
		planJobs = append(planJobs, UploadJob{Key: plan.StepKey, PlanDigest: plan.Digest})
	}
	sort.Slice(planJobs, func(i, j int) bool { return planJobs[i].Key < planJobs[j].Key })
	if expected.phase != "expected" || completed.phase != "completed" ||
		expected.importerKey == "" || expected.importerKey != completed.importerKey ||
		expected.pipelineDigest != digest || completed.pipelineDigest != digest ||
		expected.key != fmt.Sprintf("buildkite-gha/v1/uploads/%s/expected", expected.importerKey) ||
		completed.key != fmt.Sprintf("buildkite-gha/v1/uploads/%s/completed", expected.importerKey) ||
		!slices.Equal(expected.jobs, completed.jobs) || !slices.Equal(expected.jobs, planJobs) ||
		expected.value == "" || completed.value == "" {
		return fmt.Errorf("signed upload markers do not match pipeline and importer")
	}
	return nil
}
