package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
)

// environmentSourceFromAgent builds the compiler's EnvironmentSource over the
// job-scoped Agent API endpoint github-actions/environments. The Buildkite
// backend performs the GitHub reads with its own credentials, restricted to
// the pipeline's configured repository; no GitHub token reaches the importer.
// It returns nil when the job's Agent connection is not configured, leaving
// workflows that declare environments to fail closed at compile time.
func environmentSourceFromAgent(clientVersion string) compiler.EnvironmentSource {
	resolver, err := gharuntime.NewAgentEnvironmentResolver(gharuntime.AgentEnvironmentResolverConfig{
		Endpoint:      os.Getenv("BUILDKITE_AGENT_ENDPOINT"),
		JobID:         os.Getenv("BUILDKITE_JOB_ID"),
		JobToken:      os.Getenv("BUILDKITE_AGENT_ACCESS_TOKEN"),
		ClientVersion: clientVersion,
	})
	if err != nil {
		return nil
	}
	return &agentEnvironmentSource{resolver: resolver, resolved: map[string]agentEnvironmentResolution{}}
}

// seedUploadEnvironmentResolutions resolves every environment the upload's
// compilable workflows declare in one Agent API batch before per-workflow
// compilation. Workflows compile separately, so without this seeding each
// workflow's disjoint environments would consume their own request against
// the backend's per-job budget; seeding memoizes every resolution — success
// or failure — on the shared source, so one upload consumes one request and
// each workflow's compile attributes any failure to its jobs. Errors are
// deliberately not reported here: compilation surfaces them per job.
func seedUploadEnvironmentResolutions(ctx context.Context, source compiler.EnvironmentSource, workflows []workflowInput, validations []compiler.Report, processingReports []compatibility.ProcessingReport, event compiler.Event) {
	batch, ok := source.(compiler.EnvironmentBatchSource)
	if !ok {
		return
	}
	seen := map[string]bool{}
	var names []string
	for i, input := range workflows {
		if !input.Applicable || processingReportHasErrors(processingReports[i]) {
			continue
		}
		for _, name := range validations[i].BatchEnvironmentNames(event) {
			identity := strings.ToLower(name)
			if seen[identity] {
				continue
			}
			seen[identity] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	_, _ = batch.ResolveEnvironments(ctx, event.Repository.Owner, event.Repository.Name, names)
}

// agentEnvironmentSource resolves environments through the Agent API and
// memoizes every result, including failures, so repeated compiles of the same
// environment across workflows resolve once and count once against the
// backend's per-job request budget. GitHub environment names are
// case-insensitive, so Production and production memoize to one resolution.
// Every backend failure fails the compile: deployment protection never
// degrades to an unprotected default.
//
// One source spans every workflow compiled in a run, so it also rejects
// distinct environment names that collide on the same Buildkite secret prefix
// across workflows; the compiler applies the same check within each workflow.
type agentEnvironmentSource struct {
	resolver *gharuntime.AgentEnvironmentResolver

	mu       sync.Mutex
	resolved map[string]agentEnvironmentResolution
	prefixes map[string]string
}

type agentEnvironmentResolution struct {
	protection compiler.EnvironmentProtection
	err        error
}

func (a *agentEnvironmentSource) ResolveEnvironment(ctx context.Context, owner, repository, name string) (compiler.EnvironmentProtection, error) {
	protections, err := a.ResolveEnvironments(ctx, owner, repository, []string{name})
	if err != nil {
		return compiler.EnvironmentProtection{}, err
	}
	return protections[0], nil
}

// ResolveEnvironments resolves the given distinct environments, requesting
// only names without a memoized result in exactly one Agent API batch. The
// backend owns batch-size and budget policy, so oversized compilations reach
// it unsplit and its rejection fails the compile. A request failure memoizes
// that failure for every name it carried, so retried compiles fail
// consistently without new requests.
func (a *agentEnvironmentSource) ResolveEnvironments(ctx context.Context, owner, repository string, names []string) ([]compiler.EnvironmentProtection, error) {
	repo := owner + "/" + repository
	a.mu.Lock()
	var missing []string
	seen := map[string]bool{}
	for _, name := range names {
		if err := a.recordPrefix(name); err != nil {
			a.mu.Unlock()
			return nil, err
		}
		identity := strings.ToLower(name)
		if _, ok := a.resolved[repo+"\x00"+identity]; !ok && !seen[identity] {
			seen[identity] = true
			missing = append(missing, name)
		}
	}
	a.mu.Unlock()
	if len(missing) != 0 {
		snapshots, err := a.resolver.ResolveEnvironments(ctx, repo, missing)
		if err != nil {
			err = fmt.Errorf("environments: %w", err)
		} else if len(snapshots) != len(missing) {
			err = fmt.Errorf("environments: resolution returned %d environments for %d names", len(snapshots), len(missing))
		}
		a.mu.Lock()
		for i, name := range missing {
			result := agentEnvironmentResolution{err: err}
			if err == nil {
				snapshot := snapshots[i]
				result = agentEnvironmentResolution{protection: compiler.EnvironmentProtection{
					RequiredReviewers: snapshot.RequiredReviewers,
					PreventSelfReview: snapshot.PreventSelfReview,
					WaitTimerMinutes:  snapshot.WaitTimerMinutes,
					BranchPolicy:      snapshot.BranchPolicy,
					UnsupportedRules:  snapshot.UnsupportedRules,
					SecretNames:       snapshot.SecretNames,
					Variables:         snapshot.Variables,
				}}
			}
			a.resolved[repo+"\x00"+strings.ToLower(name)] = result
		}
		a.mu.Unlock()
	}
	protections := make([]compiler.EnvironmentProtection, 0, len(names))
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range names {
		result, ok := a.resolved[repo+"\x00"+strings.ToLower(name)]
		if !ok {
			return nil, fmt.Errorf("environments: environment %q was not resolved", name)
		}
		if result.err != nil {
			return nil, result.err
		}
		protections = append(protections, result.protection)
	}
	return protections, nil
}

// recordPrefix fails closed when two distinct environment names resolve to
// the same Buildkite secret prefix, which would let their secrets alias. The
// caller holds a.mu.
func (a *agentEnvironmentSource) recordPrefix(name string) error {
	prefix := compiler.EnvironmentSecretPrefix(name)
	if previous, exists := a.prefixes[prefix]; exists && !strings.EqualFold(previous, name) {
		return fmt.Errorf("environments %q and %q both resolve to Buildkite secret prefix %q; rename one so environment-scoped secrets stay distinct", previous, name, prefix)
	}
	if a.prefixes == nil {
		a.prefixes = map[string]string{}
	}
	a.prefixes[prefix] = name
	return nil
}
