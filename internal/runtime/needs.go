package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
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
		needs[name] = plan.Need{Result: result.Result, Outputs: result.Outputs}
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
	}
	if _, err := transport.MarshalResultManifest(manifest); err != nil {
		fallback := manifest
		fallback.Result = "failure"
		if result.Conclusion == "cancelled" {
			fallback.Result = "cancelled"
		}
		fallback.Outputs = nil
		publication, publishErr := transport.PublishResult(ctx, agent, root, workflow, instance, fallback)
		if publishErr != nil {
			return publication, errors.Join(fmt.Errorf("validate terminal result: %w", err), fmt.Errorf("publish bounded terminal result: %w", publishErr))
		}
		return publication, fmt.Errorf("validate terminal result: %w", err)
	}
	return transport.PublishResult(ctx, agent, root, workflow, instance, manifest)
}
