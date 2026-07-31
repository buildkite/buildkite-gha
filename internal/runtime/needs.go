package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const (
	jobSummaryAnnotationContext = "buildkite-gha-job-summary"
	jobWarningAnnotationContext = "buildkite-gha-workflow-warnings"
	jobErrorAnnotationContext   = "buildkite-gha-workflow-errors"
)

// ResolveNeeds converts compiler-owned producer identities into the verified
// logical results and outputs consumed by runtime expression contexts.
func ResolveNeeds(ctx context.Context, agent transport.Agent, root, buildID string, sources map[string][]plan.NeedSource) (map[string]plan.Need, error) {
	transportSources := make(map[string][]transport.ResultSource, len(sources))
	for name, producers := range sources {
		for _, producer := range producers {
			transportSources[name] = append(transportSources[name], transport.ResultSource{
				StepKey: producer.StepKey, PlanDigest: producer.PlanDigest,
			})
		}
	}
	verified, err := transport.LoadNeeds(ctx, agent, root, buildID, transportSources)
	if err != nil {
		return nil, err
	}
	needs := make(map[string]plan.Need, len(verified))
	for name, result := range verified {
		need := plan.Need{Result: result.Result, Outputs: result.Outputs}
		for _, retained := range result.Artifacts {
			a, p := retained.Artifact, retained.Producer
			need.Artifacts = append(need.Artifacts, plan.NeedArtifact{Name: a.Name, ID: a.ID, Path: a.Path, Digest: a.Digest, Size: a.Size, FileCount: a.FileCount, Producer: plan.NeedProducer{BuildID: p.BuildID, JobID: p.JobID, StepKey: p.StepKey}})
		}
		needs[name] = need
	}
	return needs, nil
}

// PublishJobResult maps every terminal runtime conclusion to the canonical
// producer-attributed manifest. The caller invokes it for success, failure,
// cancelled, and runtime-skipped jobs before exiting.
func PublishJobResult(ctx context.Context, agent transport.Agent, root, workflow, instance, planDigest string, producer transport.Producer, result JobResult) (transport.Publication, error) {
	outputs := make([]transport.Output, 0, len(result.Outputs))
	for name, value := range result.Outputs {
		outputs = append(outputs, transport.Output{Name: name, Value: value})
	}
	manifest := transport.ResultManifest{
		PlanDigest: planDigest,
		Producer:   producer,
		Result:     result.Conclusion,
		Outputs:    outputs,
		Artifacts:  append([]transport.ResultArtifact(nil), result.Artifacts...),
	}
	if _, err := transport.MarshalResultManifest(manifest); err != nil {
		fallback := manifest
		fallback.Result = "failure"
		if result.Conclusion == "cancelled" {
			fallback.Result = "cancelled"
		}
		fallback.Outputs = nil
		fallback.Artifacts = nil
		publication, publishErr := transport.PublishResult(ctx, agent, root, workflow, instance, fallback)
		if publishErr != nil {
			return publication, errors.Join(fmt.Errorf("validate terminal result: %w", err), fmt.Errorf("publish bounded terminal result: %w", publishErr))
		}
		publishJobAnnotations(ctx, agent, producer.JobID, result, &publication)
		return publication, fmt.Errorf("validate terminal result: %w", err)
	}
	publication, err := transport.PublishResult(ctx, agent, root, workflow, instance, manifest)
	if err != nil {
		return publication, err
	}
	publishJobAnnotations(ctx, agent, producer.JobID, result, &publication)
	return publication, nil
}

func publishJobAnnotations(ctx context.Context, agent transport.Agent, jobID string, result JobResult, publication *transport.Publication) {
	publishJobSummary(ctx, agent, jobID, result.Summary, publication)
	if result.WarningAnnotations != "" {
		if err := agent.AnnotateJob(ctx, jobID, jobWarningAnnotationContext, "warning", result.WarningAnnotations); err != nil {
			publication.WarningAnnotationError = fmt.Errorf("publish workflow warnings: %w", err)
		}
	}
	if result.ErrorAnnotations != "" {
		if err := agent.AnnotateJob(ctx, jobID, jobErrorAnnotationContext, "error", result.ErrorAnnotations); err != nil {
			publication.ErrorAnnotationError = fmt.Errorf("publish workflow errors: %w", err)
		}
	}
}

func publishJobSummary(ctx context.Context, agent transport.Agent, jobID, summary string, publication *transport.Publication) {
	if summary == "" {
		return
	}
	if err := agent.AnnotateJob(ctx, jobID, jobSummaryAnnotationContext, "info", summary); err != nil {
		publication.SummaryAnnotationError = fmt.Errorf("publish job summary: %w", err)
	}
}
