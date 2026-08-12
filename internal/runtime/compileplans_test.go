package runtime

import (
	"context"

	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// compilePlansForTest compiles plans through the production
// compiler.CompileBundlePlansContext path and unwraps the plan jobs, so tests
// exercise the interface that ships rather than a test-only wrapper.
func compilePlansForTest(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest string, options compiler.Options) ([]plan.Job, error) {
	bundle, err := compiler.CompileBundlePlansContext(ctx, path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
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
	options := compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels: map[string]string{
				"ubuntu-latest": targetQueue,
				"ubuntu-24.04":  targetQueue,
				"ubuntu-22.04":  targetQueue,
			},
			UntrustedQueues: []string{targetQueue},
		},
	}
	return compilePlansForTest(context.Background(), path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
}
