package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// EnvironmentProtection is the compile-time snapshot of one GitHub deployment
// environment: its protection rules and the names of its environment secrets.
// Secret values never reach the compiler; they stay in Buildkite Secrets.
type EnvironmentProtection struct {
	// RequiredReviewers reports whether the environment requires manual
	// approval before deployment jobs run.
	RequiredReviewers bool
	// PreventSelfReview reports GitHub's prevent-self-review reviewer setting,
	// which Buildkite block steps cannot enforce.
	PreventSelfReview bool
	// WaitTimerMinutes is the environment's wait timer. Any positive value is
	// rejected at compile time.
	WaitTimerMinutes int
	// BranchPolicy reports whether the environment restricts deployment
	// branches. Branch policies are rejected at compile time.
	BranchPolicy bool
	// UnsupportedRules names protection rules the compiler does not recognize,
	// such as custom deployment protection rules. Any entry is rejected at
	// compile time.
	UnsupportedRules []string
	// SecretNames are the environment's secret names.
	SecretNames []string
	// Variables are the environment's variables by name with their plaintext
	// values. They are configuration, not secrets, but compile diagnostics
	// must still name variables without printing values.
	Variables map[string]string
}

// EnvironmentSource resolves one GitHub deployment environment by name.
// Callers should share one memoized source for the complete validate,
// compile, and upload operation.
type EnvironmentSource interface {
	ResolveEnvironment(ctx context.Context, owner, repository, name string) (EnvironmentProtection, error)
}

// EnvironmentBatchSource resolves several distinct environments together,
// returning protections in request order. Sources implement it when their
// backend accounts for resolution per request, so one workflow's
// environments resolve in one request instead of one request per name. Any
// error fails the whole batch.
type EnvironmentBatchSource interface {
	ResolveEnvironments(ctx context.Context, owner, repository string, names []string) ([]EnvironmentProtection, error)
}

type environmentResolution struct {
	protection EnvironmentProtection
	err        error
}

// resolveJobEnvironments resolves every declared job environment against the
// event repository and records approval requirements and environment secret
// names on each instance. Unsupported protection rules fail closed so a
// deployment job never runs with weaker protection than GitHub would apply.
func resolveJobEnvironments(ctx context.Context, instances []JobInstance, event Event, options Options) error {
	resolved := seedBatchResolutions(ctx, instances, event, options)
	prefixes := map[string]string{}
	var errs []error
	for i := range instances {
		instance := &instances[i]
		if instance.Environment == "" {
			continue
		}
		finding := func(err error) {
			errs = append(errs, attributedProcessingFinding(StageGraph, CodeGraphInvalid, "compatibility", instance.SourcePath, instance.Source.Start.Line, instance.Source.Start.Column, instance.LogicalJobID, instance.Key, "", 0, err))
		}
		if instance.reusableCall.Line != 0 {
			finding(fmt.Errorf("job %q declares environment %q inside a reusable workflow; environments are only supported on jobs of the top-level workflow", instance.LogicalJobID, instance.Environment))
			continue
		}
		if options.EnvironmentSource == nil {
			finding(fmt.Errorf("job %q declares environment %q, but environments resolve only through the job-scoped Buildkite Agent API; run this command in a Buildkite job, or remove the environment", instance.LogicalJobID, instance.Environment))
			continue
		}
		if event.Provider != "github" {
			finding(fmt.Errorf("job %q declares environment %q, but environments require a GitHub event repository", instance.LogicalJobID, instance.Environment))
			continue
		}
		// GitHub environment names are case-insensitive: Production and
		// production are one environment, so they share a resolution and
		// never count as a prefix collision.
		prefix := EnvironmentSecretPrefix(instance.Environment)
		if previous, exists := prefixes[prefix]; exists && !strings.EqualFold(previous, instance.Environment) {
			finding(fmt.Errorf("environments %q and %q both resolve to Buildkite secret prefix %q; rename one so environment-scoped secrets stay distinct", previous, instance.Environment, prefix))
			continue
		}
		prefixes[prefix] = instance.Environment
		identity := strings.ToLower(instance.Environment)
		result, ok := resolved[identity]
		if !ok {
			protection, err := options.EnvironmentSource.ResolveEnvironment(ctx, event.Repository.Owner, event.Repository.Name, instance.Environment)
			result = environmentResolution{protection: protection, err: err}
			resolved[identity] = result
		}
		if result.err != nil {
			finding(fmt.Errorf("job %q environment %q: %w", instance.LogicalJobID, instance.Environment, result.err))
			continue
		}
		if err := unsupportedEnvironmentProtection(instance.Environment, result.protection); err != nil {
			finding(fmt.Errorf("job %q: %w", instance.LogicalJobID, err))
			continue
		}
		instance.EnvironmentApproval = result.protection.RequiredReviewers
		instance.environmentSecrets = append([]string(nil), result.protection.SecretNames...)
		instance.environmentVariables = cloneMap(result.protection.Variables)
	}
	return errors.Join(errs...)
}

// seedBatchResolutions pre-resolves, in one batch request, exactly the
// distinct environment names the per-instance loop would resolve, so the loop
// finds every resolution memoized. Names skipped by the loop's earlier checks
// (reusable workflows, non-GitHub events, secret-prefix collisions) are
// excluded so a batch never fails on a name that would not resolve. A batch
// failure seeds every requested name with that error, keeping resolution
// fail-closed with per-job attribution.
func seedBatchResolutions(ctx context.Context, instances []JobInstance, event Event, options Options) map[string]environmentResolution {
	resolved := map[string]environmentResolution{}
	batch, ok := options.EnvironmentSource.(EnvironmentBatchSource)
	if !ok {
		return resolved
	}
	names := batchResolutionNames(instances, event)
	for _, name := range names {
		resolved[strings.ToLower(name)] = environmentResolution{}
	}
	if len(names) == 0 {
		return resolved
	}
	protections, err := batch.ResolveEnvironments(ctx, event.Repository.Owner, event.Repository.Name, names)
	if err == nil && len(protections) != len(names) {
		err = fmt.Errorf("environment resolution returned %d environments for %d names", len(protections), len(names))
	}
	for i, name := range names {
		result := environmentResolution{err: err}
		if err == nil {
			result = environmentResolution{protection: protections[i]}
		}
		resolved[strings.ToLower(name)] = result
	}
	return resolved
}

// batchResolutionNames returns the distinct environment names one batched
// resolution request would carry for these instances, in declaration order,
// with the first spelling of case-insensitive duplicates. Names skipped by
// resolution's earlier checks (reusable workflows, non-GitHub events,
// secret-prefix collisions) are excluded so a batch never fails on a name
// that would not resolve.
func batchResolutionNames(instances []JobInstance, event Event) []string {
	if event.Provider != "github" {
		return nil
	}
	prefixes := map[string]string{}
	seen := map[string]bool{}
	var names []string
	for i := range instances {
		instance := &instances[i]
		if instance.Environment == "" || instance.reusableCall.Line != 0 {
			continue
		}
		prefix := EnvironmentSecretPrefix(instance.Environment)
		if previous, exists := prefixes[prefix]; exists {
			if !strings.EqualFold(previous, instance.Environment) {
				continue // resolution reports the prefix collision
			}
		} else {
			prefixes[prefix] = instance.Environment
		}
		identity := strings.ToLower(instance.Environment)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		names = append(names, instance.Environment)
	}
	return names
}

// BatchEnvironmentNames returns the distinct environment names one compile of
// the reported workflow would resolve, applying the same exclusions as
// resolution itself. Not-evaluated instances never resolve, so they are
// excluded. Callers can union several workflows' names to resolve one
// upload's environments in one batched request before compiling.
func (r Report) BatchEnvironmentNames(event Event) []string {
	evaluated := make([]JobInstance, 0, len(r.Jobs))
	for _, instance := range r.Jobs {
		if r.NotEvaluatedInstances[instance.Key] {
			continue
		}
		evaluated = append(evaluated, instance)
	}
	return batchResolutionNames(evaluated, event)
}

func unsupportedEnvironmentProtection(name string, protection EnvironmentProtection) error {
	if protection.WaitTimerMinutes > 0 {
		return fmt.Errorf("environment %q sets a %d-minute wait timer; wait timers are unsupported because simulating them would occupy a Buildkite agent, so remove the wait timer or keep this workflow on GitHub", name, protection.WaitTimerMinutes)
	}
	if protection.BranchPolicy {
		return fmt.Errorf("environment %q restricts deployment branches; deployment branch policies are unsupported, so remove the branch policy or keep this workflow on GitHub", name)
	}
	if len(protection.UnsupportedRules) != 0 {
		return fmt.Errorf("environment %q uses unsupported protection rules (%s); custom deployment protection rules are unsupported, so remove them or keep this workflow on GitHub", name, strings.Join(protection.UnsupportedRules, ", "))
	}
	return nil
}

// environmentScopedSecrets applies GitHub's environment-over-repository secret
// precedence to one job's referenced secret names. A referenced name that the
// environment defines resolves to the Buildkite secret named after the
// environment and the secret, so different environments keep distinct values.
func environmentScopedSecrets(environment string, environmentSecrets, secrets []string, mappings map[string]string) ([]string, map[string]string, error) {
	if len(mappings) != 0 {
		// Reusable-workflow secret authority is rejected earlier for
		// environment jobs; refuse to compose the two rewrites.
		return nil, nil, fmt.Errorf("environment %q cannot rescope already-mapped secrets", environment)
	}
	scoped := make(map[string]bool, len(environmentSecrets))
	for _, name := range environmentSecrets {
		scoped[strings.ToUpper(name)] = true
	}
	prefix := EnvironmentSecretPrefix(environment)
	rewritten := false
	scopedMappings := make(map[string]string, len(secrets))
	sources := make(map[string]string, len(secrets))
	ordered := make([]string, 0, len(secrets))
	for _, alias := range secrets {
		source := alias
		if scoped[strings.ToUpper(alias)] {
			source = prefix + "_" + strings.ToUpper(alias)
			if err := validateEnvironmentSecretKey(environment, alias, source); err != nil {
				return nil, nil, err
			}
			rewritten = true
		}
		if previous, exists := sources[strings.ToUpper(source)]; exists {
			return nil, nil, fmt.Errorf("environment %q secrets %q and %q both resolve to Buildkite secret %q; rename one so every secret reference resolves to a distinct Buildkite secret", environment, previous, alias, source)
		}
		sources[strings.ToUpper(source)] = alias
		scopedMappings[alias] = source
		ordered = append(ordered, source)
	}
	if !rewritten {
		return secrets, nil, nil
	}
	sort.Strings(ordered)
	return ordered, scopedMappings, nil
}

// buildkiteSecretKeyLimit is Buildkite Secrets' maximum key length.
const buildkiteSecretKeyLimit = 255

// validateEnvironmentSecretKey rejects generated environment-scoped keys that
// Buildkite Secrets cannot store: keys are limited to 255 characters and
// cannot begin with "bk" or "buildkite" in any case. Failing at compile time
// beats failing later in buildkite-agent secret get.
func validateEnvironmentSecretKey(environment, name, key string) error {
	if strings.HasPrefix(key, "BK") || strings.HasPrefix(key, "BUILDKITE") {
		return fmt.Errorf("environment %q secret %q resolves to Buildkite secret %q, but Buildkite secret keys cannot begin with BK or BUILDKITE; rename the environment so its secrets resolve to storable keys", environment, name, key)
	}
	if len(key) > buildkiteSecretKeyLimit {
		return fmt.Errorf("environment %q secret %q resolves to Buildkite secret %q, which exceeds Buildkite's %d-character secret key limit; shorten the environment or secret name", environment, name, key, buildkiteSecretKeyLimit)
	}
	return nil
}

// EnvironmentSecretPrefix is the Buildkite Secrets naming convention for
// environment-scoped secrets: the upper-cased environment name with every
// character outside [A-Z0-9_] replaced by an underscore. Secret DEPLOY_KEY in
// environment production resolves to Buildkite secret PRODUCTION_DEPLOY_KEY.
// The replacement is lossy, so distinct environment names sharing one prefix
// are rejected wherever environments are resolved.
func EnvironmentSecretPrefix(environment string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(environment) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	prefix := out.String()
	if prefix == "" || prefix[0] >= '0' && prefix[0] <= '9' {
		prefix = "_" + prefix
	}
	return prefix
}

// environmentGateKey deterministically names the one approval block emitted
// for every environment that requires reviewers, in the same key namespace as
// generated job keys.
func environmentGateKey(namespace, environment string) string {
	prefix := "gha-"
	if namespace != "" {
		prefix += namespace + "-"
	}
	var out strings.Builder
	for _, r := range strings.ToLower(environment) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		default:
			if out.Len() > 0 {
				out.WriteByte('-')
			}
		}
	}
	sanitized := strings.Trim(out.String(), "-")
	if len(sanitized) > 64 {
		sanitized = strings.Trim(sanitized[:64], "-")
	}
	// Case-fold the digest input so case variants of one GitHub environment
	// share a gate key.
	digest := sha256.Sum256([]byte("environment\x00" + strings.ToLower(environment)))
	suffix := hex.EncodeToString(digest[:6])
	if sanitized == "" {
		sanitized = "environment"
	}
	return prefix + "approve-" + sanitized + "-" + suffix
}
