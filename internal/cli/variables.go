package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

// variableSource resolves a repository's GitHub Actions repository and
// organization variables into the compiler's pre-environment scopes.
type variableSource interface {
	ResolveVariables(ctx context.Context, owner, repository string) (compiler.VariableSources, error)
}

// variableSourceFromAgent builds the variable source over the job-scoped Agent
// API endpoint github-actions/variables. The Buildkite backend performs the
// GitHub reads with its own credentials, restricted to the pipeline's
// configured repository; no GitHub token reaches the importer. It returns nil
// when the job's Agent connection is not configured, in which case vars keep
// their empty pre-environment scopes.
func variableSourceFromAgent(clientVersion string) variableSource {
	resolver, err := gharuntime.NewAgentVariableResolver(gharuntime.AgentVariableResolverConfig{
		Endpoint:      os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
		JobID:         os.Getenv("BUILDKITE_JOB_ID"),
		JobToken:      os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
		ClientVersion: clientVersion,
	})
	if err != nil {
		return nil
	}
	return &agentVariableSource{resolver: resolver, resolved: map[string]agentVariableResolution{}}
}

// agentVariableSource resolves variables through the Agent API and memoizes
// every result, including failures, per repository, so one upload of many
// workflows consumes one request against the backend's per-job budget and
// retried compiles fail consistently without new requests. A 404 means the
// backend offers no repository or organization scope; it memoizes as empty
// scopes so names no scope defines keep evaluating as empty strings.
type agentVariableSource struct {
	resolver *gharuntime.AgentVariableResolver

	mu       sync.Mutex
	resolved map[string]agentVariableResolution
}

type agentVariableResolution struct {
	vars compiler.VariableSources
	err  error
}

func (a *agentVariableSource) ResolveVariables(ctx context.Context, owner, repository string) (compiler.VariableSources, error) {
	repo := owner + "/" + repository
	a.mu.Lock()
	defer a.mu.Unlock()
	if result, ok := a.resolved[repo]; ok {
		return result.vars, result.err
	}
	snapshot, err := a.resolver.ResolveVariables(ctx, repo)
	result := agentVariableResolution{}
	switch {
	case errors.Is(err, gharuntime.ErrVariablesUnavailable):
	case err != nil:
		result.err = fmt.Errorf("variables: %w", err)
	default:
		result.vars = compiler.VariableSources{Repository: snapshot.Repository, Organization: snapshot.Organization}
	}
	a.resolved[repo] = result
	return result.vars, result.err
}

// resolveVariableSources fills the compiler's pre-environment vars scopes for
// the event repository when the workflows about to compile reference vars.
// Without a reference there is nothing to resolve, so no request is made and
// the scopes stay empty. Without a source, such as compile outside a
// Buildkite job, the scopes also stay empty. Only GitHub.com repositories
// have GitHub variables, so events from other providers never make a request.
func resolveVariableSources(ctx context.Context, source variableSource, event compiler.Event, referencesVars bool) (compiler.VariableSources, error) {
	if source == nil || !referencesVars || event.Provider != "github" {
		return compiler.VariableSources{}, nil
	}
	return source.ResolveVariables(ctx, event.Repository.Owner, event.Repository.Name)
}

// resolveActionVariables handles a vars reference that only compilation can
// discover: an input default in a resolved action's metadata. When the
// workflow itself references no vars, the scopes were never requested, so a
// bundle whose action programs read vars requests them now, still at most
// once per upload, and reports whether the caller must compile again so the
// plans carry the scopes. The workflow references no vars, so compiling again
// changes nothing but those scopes.
func resolveActionVariables(ctx context.Context, source variableSource, event compiler.Event, workflowReferencesVars bool, bundle compiler.Bundle) (vars compiler.VariableSources, again bool, err error) {
	if workflowReferencesVars || source == nil || !compiler.ActionsReferenceVars(bundle) {
		return compiler.VariableSources{}, false, nil
	}
	vars, err = resolveVariableSources(ctx, source, event, true)
	if err != nil {
		return compiler.VariableSources{}, false, err
	}
	return vars, len(vars.Repository) != 0 || len(vars.Organization) != 0, nil
}

// resolveUploadVariables resolves the vars scopes once for every applicable
// workflow that references vars, before validation so compile-time fields
// see them. A resolution failure fails each workflow that references vars
// with the backend's error, leaving other workflows to upload; the value
// scopes never join a diagnostic.
func resolveUploadVariables(ctx context.Context, source variableSource, workflows []workflowInput, processingReports []compatibility.ProcessingReport, event compiler.Event) compiler.VariableSources {
	referencesVars := false
	for i, input := range workflows {
		if input.Applicable && !processingReportHasErrors(processingReports[i]) && input.ReferencesVars {
			referencesVars = true
		}
	}
	vars, err := resolveVariableSources(ctx, source, event, referencesVars)
	if err == nil {
		return vars
	}
	for i, input := range workflows {
		if !input.Applicable || processingReportHasErrors(processingReports[i]) || !input.ReferencesVars {
			continue
		}
		processingReports[i] = triggerProcessingReport(input.Path, input.Source)
		processingReports[i].AddEnvironmentFailure(err.Error())
		processingReports[i].Result = "indeterminate"
	}
	return compiler.VariableSources{}
}
