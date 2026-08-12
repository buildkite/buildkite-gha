package compiler

import (
	"context"

	"github.com/buildkite/buildkite-gha/internal/plan"
)

// compilePlansForTest compiles plans through the production
// CompileBundlePlansContext path and unwraps the plan jobs, so tests exercise
// the interface that ships rather than a test-only wrapper.
func compilePlansForTest(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest string, options Options) ([]plan.Job, error) {
	bundle, err := CompileBundlePlansContext(ctx, path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
	if err != nil {
		return nil, err
	}
	jobs := make([]plan.Job, len(bundle.Plans))
	for i, artifact := range bundle.Plans {
		jobs[i] = artifact.Job
	}
	return jobs, nil
}

// compileUntrustedPlans applies the tokenless untrusted convenience policy:
// every default runner label pinned to targetQueue, which is also the only
// queue admitted for untrusted events.
func compileUntrustedPlans(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, targetQueue string) ([]plan.Job, error) {
	options := defaultOptions()
	if targetQueue != "" {
		for label := range options.Runners.Labels {
			options.Runners.Labels[label] = targetQueue
		}
		options.Runners.UntrustedQueues = []string{targetQueue}
		options.Runners.AllowUntrustedDefaultQueue = false
	}
	return compilePlansForTest(context.Background(), path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
}
