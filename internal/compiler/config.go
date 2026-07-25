package compiler

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var queuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// EventTrust is the compiler's authenticated classification of an event.
type EventTrust string

const (
	EventTrusted   EventTrust = "trusted"
	EventUntrusted EventTrust = "untrusted"
)

// VariableSources are explicit non-secret inputs to the vars context.
// Precedence is Bridge < Provider < Buildkite, so the build-specific snapshot
// wins over repository defaults without reading arbitrary process environment.
type VariableSources struct {
	Bridge    map[string]string
	Provider  map[string]string
	Buildkite map[string]string
}

// RunnerPolicy maps every accepted Linux runner label to a Buildkite queue.
// Multi-label targets are accepted only when every label maps to the same
// queue. Untrusted events are additionally restricted to UntrustedQueues.
type RunnerPolicy struct {
	Labels          map[string]string
	UntrustedQueues []string
}

// Options are the complete non-secret graph-construction inputs beyond the
// workflow and canonical event snapshot.
type Options struct {
	Vars       VariableSources
	Runners    RunnerPolicy
	EventTrust EventTrust
	// ResolveActions enables immutable remote action locking independently of
	// event trust. Workspace-local actions are always locked without network
	// access. ActionSource is required only when a workflow uses remote actions.
	ResolveActions     bool
	ActionSource       ActionSource
	NodeRuntimeDigests map[int]string
}

func defaultOptions() Options {
	return Options{
		// The convenience wrappers accept a local event path with no attestation,
		// so their fixed policy is tokenless and untrusted by construction.
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{Labels: map[string]string{
			"ubuntu-latest": "gha-untrusted",
			"ubuntu-24.04":  "gha-untrusted",
			"ubuntu-22.04":  "gha-untrusted",
		}, UntrustedQueues: []string{"gha-untrusted"}},
	}
}

func (options Options) validate() error {
	if options.EventTrust != EventTrusted && options.EventTrust != EventUntrusted {
		return fmt.Errorf("event trust must be %q or %q", EventTrusted, EventUntrusted)
	}
	if !options.ResolveActions && options.ActionSource != nil {
		return fmt.Errorf("action source configuration requires ResolveActions")
	}
	if len(options.Runners.Labels) == 0 {
		return fmt.Errorf("runner policy requires at least one label mapping")
	}
	labels := make(map[string]string, len(options.Runners.Labels))
	for label, queue := range options.Runners.Labels {
		if strings.TrimSpace(label) == "" || strings.TrimSpace(queue) == "" {
			return fmt.Errorf("runner policy labels and queues must be non-empty")
		}
		if !queuePattern.MatchString(queue) {
			return fmt.Errorf("runner policy queue %q is invalid", queue)
		}
		normalized := strings.ToLower(strings.TrimSpace(label))
		if existing, ok := labels[normalized]; ok && existing != queue {
			return fmt.Errorf("runner label %q has conflicting queue mappings", label)
		}
		labels[normalized] = queue
	}
	for _, queue := range options.Runners.UntrustedQueues {
		if !queuePattern.MatchString(queue) {
			return fmt.Errorf("untrusted queue allowlist contains invalid queue %q", queue)
		}
	}
	for _, varsSource := range []struct {
		name   string
		values map[string]string
	}{
		{name: "bridge", values: options.Vars.Bridge},
		{name: "provider", values: options.Vars.Provider},
		{name: "buildkite", values: options.Vars.Buildkite},
	} {
		sourceName, source := varsSource.name, varsSource.values
		names := make(map[string]string, len(source))
		for name := range source {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("vars source contains an empty variable name")
			}
			normalized := strings.ToUpper(name)
			if existing, ok := names[normalized]; ok {
				return fmt.Errorf("%s vars source contains case-colliding names %q and %q", sourceName, existing, name)
			}
			names[normalized] = name
		}
	}
	return nil
}

func (sources VariableSources) snapshot() map[string]string {
	vars := make(map[string]string, len(sources.Bridge)+len(sources.Provider)+len(sources.Buildkite))
	for _, source := range []map[string]string{sources.Bridge, sources.Provider, sources.Buildkite} {
		for name, value := range source {
			vars[strings.ToUpper(name)] = value
		}
	}
	if len(vars) == 0 {
		return nil
	}
	return vars
}

func (policy RunnerPolicy) resolve(labels []string, trust EventTrust) (string, error) {
	if len(labels) == 0 {
		return "", fmt.Errorf("runs-on must resolve to at least one label")
	}
	var queue string
	for _, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if unsupportedOS(normalized) {
			return "", fmt.Errorf("unsupported operating system runner label %q", label)
		}
		mapped, ok := policy.Labels[normalized]
		if !ok {
			for configured, candidate := range policy.Labels {
				if strings.ToLower(strings.TrimSpace(configured)) == normalized {
					mapped, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			return "", fmt.Errorf("runner label %q is not mapped by policy", label)
		}
		if queue != "" && queue != mapped {
			return "", fmt.Errorf("runner labels resolve to conflicting queues %q and %q", queue, mapped)
		}
		queue = mapped
	}
	if trust == EventUntrusted && !contains(policy.UntrustedQueues, queue) {
		allowlist := append([]string(nil), policy.UntrustedQueues...)
		sort.Strings(allowlist)
		return "", fmt.Errorf("untrusted event cannot target queue %q; allowed queues: %s", queue, strings.Join(allowlist, ", "))
	}
	return queue, nil
}

func unsupportedOS(label string) bool {
	return strings.HasPrefix(label, "windows-") || strings.HasPrefix(label, "macos-") || label == "windows" || label == "macos"
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
