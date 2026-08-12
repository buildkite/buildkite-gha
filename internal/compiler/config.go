package compiler

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var queuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var distributionDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var runtimeImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)

// OperatingSystem identifies one supported workflow host operating system.
type OperatingSystem string

const (
	OperatingSystemLinux  OperatingSystem = "linux"
	OperatingSystemDarwin OperatingSystem = "darwin"
)

// Architecture identifies one supported workflow host architecture.
type Architecture string

const (
	ArchitectureAMD64 Architecture = "amd64"
	ArchitectureARM64 Architecture = "arm64"
)

// Platform is one validated workflow execution platform.
type Platform struct {
	OS   OperatingSystem
	Arch Architecture
}

var (
	PlatformLinuxAMD64  = Platform{OS: OperatingSystemLinux, Arch: ArchitectureAMD64}
	PlatformDarwinARM64 = Platform{OS: OperatingSystemDarwin, Arch: ArchitectureARM64}
)

func (platform Platform) String() string {
	return string(platform.OS) + "/" + string(platform.Arch)
}

// ParsePlatform parses one supported operating-system and architecture pair.
func ParsePlatform(value string) (Platform, error) {
	switch value {
	case PlatformLinuxAMD64.String():
		return PlatformLinuxAMD64, nil
	case PlatformDarwinARM64.String():
		return PlatformDarwinARM64, nil
	default:
		return Platform{}, fmt.Errorf("unsupported runtime platform %q", value)
	}
}

func (platform Platform) validate() error {
	_, err := ParsePlatform(platform.String())
	return err
}

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

// RunnerTarget atomically selects one Buildkite queue, execution platform, and
// optional immutable runtime image.
type RunnerTarget struct {
	Queue    string
	Platform Platform
	Image    string
}

// RunnerPolicy maps every accepted runner label to a Buildkite queue and
// execution platform. An empty Linux queue uses Buildkite's default agent
// targeting. Darwin always requires an explicit queue. Multi-label targets are
// accepted only when every label maps to the same complete target. Untrusted
// events are additionally restricted to UntrustedQueues unless the default is
// explicitly allowed.
type RunnerPolicy struct {
	// Labels retains the Linux/amd64 label-to-queue shorthand used by direct
	// compiler callers. New multi-platform policy should use Targets.
	Labels                     map[string]string
	Targets                    map[string]RunnerTarget
	UntrustedQueues            []string
	AllowUntrustedDefaultQueue bool
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
	ResolveActions bool
	ActionSource   ActionSource
	// GroupLabel merges generated jobs into the Buildkite group containing the
	// importer. It affects pipeline presentation only, not compiler IR or plans.
	GroupLabel string
	// RuntimeDistributions binds each supported workflow platform to the exact
	// executable digest generated jobs must download and execute. Linux defaults
	// to the compiler distribution for backward-compatible local compilation.
	RuntimeDistributions map[Platform]string
	// RuntimeImage selects one immutable image for generated Linux workflow jobs.
	// Darwin Hosted Agents are native VMs and never receive this image.
	RuntimeImage string
}

func defaultOptions() Options {
	return Options{
		// The convenience wrappers accept a local event path with no attestation,
		// so their policy is tokenless and untrusted by construction. Agent
		// targeting remains owned by the Buildkite pipeline configuration.
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{Labels: map[string]string{
			"ubuntu-latest": "",
			"ubuntu-24.04":  "",
			"ubuntu-22.04":  "",
		}, AllowUntrustedDefaultQueue: true},
	}
}

func (options Options) validate() error {
	if options.EventTrust != EventTrusted && options.EventTrust != EventUntrusted {
		return fmt.Errorf("event trust must be %q or %q", EventTrusted, EventUntrusted)
	}
	if !options.ResolveActions && options.ActionSource != nil {
		return fmt.Errorf("action source configuration requires ResolveActions")
	}
	if len(options.Runners.Labels) == 0 && len(options.Runners.Targets) == 0 {
		return fmt.Errorf("runner policy requires at least one label mapping")
	}
	labels := make(map[string]RunnerTarget, len(options.Runners.Labels)+len(options.Runners.Targets))
	for label, queue := range options.Runners.Labels {
		if err := validateRunnerTarget(labels, label, RunnerTarget{Queue: queue, Platform: PlatformLinuxAMD64}); err != nil {
			return err
		}
	}
	for label, target := range options.Runners.Targets {
		if err := validateRunnerTarget(labels, label, target); err != nil {
			return err
		}
	}
	for _, queue := range options.Runners.UntrustedQueues {
		if !queuePattern.MatchString(queue) {
			return fmt.Errorf("untrusted queue allowlist contains invalid queue %q", queue)
		}
	}
	for platform, digest := range options.RuntimeDistributions {
		if err := platform.validate(); err != nil {
			return err
		}
		if !distributionDigestPattern.MatchString(digest) {
			return fmt.Errorf("runtime distribution for %s has invalid digest %q", platform, digest)
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

func validateRunnerTarget(labels map[string]RunnerTarget, label string, target RunnerTarget) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("runner policy labels must be non-empty")
	}
	if target.Queue != "" && !queuePattern.MatchString(target.Queue) {
		return fmt.Errorf("runner policy queue %q is invalid", target.Queue)
	}
	if err := target.Platform.validate(); err != nil {
		return fmt.Errorf("runner label %q: %w", label, err)
	}
	if target.Platform == PlatformDarwinARM64 && target.Queue == "" {
		return fmt.Errorf("runner label %q targets darwin/arm64 without an explicit queue", label)
	}
	if target.Image != "" && !runtimeImagePattern.MatchString(target.Image) {
		return fmt.Errorf("runner label %q has invalid immutable runtime image %q", label, target.Image)
	}
	if target.Platform == PlatformDarwinARM64 && target.Image != "" {
		return fmt.Errorf("runner label %q cannot select a runtime image on darwin/arm64", label)
	}
	normalized := strings.ToLower(strings.TrimSpace(label))
	if existing, ok := labels[normalized]; ok && existing != target {
		return fmt.Errorf("runner label %q has conflicting target mappings", label)
	}
	labels[normalized] = target
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

func (policy RunnerPolicy) resolve(labels []string, trust EventTrust) (RunnerTarget, error) {
	if len(labels) == 0 {
		return RunnerTarget{}, fmt.Errorf("runs-on must resolve to at least one label")
	}
	var target RunnerTarget
	resolved := false
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if _, duplicate := seen[normalized]; duplicate {
			return RunnerTarget{}, fmt.Errorf("runs-on contains duplicate runner label %q", label)
		}
		seen[normalized] = struct{}{}
		if unsupportedOS(normalized) {
			return RunnerTarget{}, fmt.Errorf("unsupported operating system runner label %q", label)
		}
		mapped, ok := policy.Targets[normalized]
		if !ok {
			for configured, candidate := range policy.Targets {
				if strings.ToLower(strings.TrimSpace(configured)) == normalized {
					mapped, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			if queue, exists := policy.Labels[normalized]; exists {
				mapped, ok = RunnerTarget{Queue: queue, Platform: PlatformLinuxAMD64}, true
			} else {
				for configured, queue := range policy.Labels {
					if strings.ToLower(strings.TrimSpace(configured)) == normalized {
						mapped, ok = RunnerTarget{Queue: queue, Platform: PlatformLinuxAMD64}, true
						break
					}
				}
			}
		}
		if !ok {
			return RunnerTarget{}, fmt.Errorf("runner label %q is not mapped by policy", label)
		}
		if resolved && target != mapped {
			if target.Platform == mapped.Platform && target.Image == mapped.Image {
				return RunnerTarget{}, fmt.Errorf("runner labels resolve to conflicting queues %q and %q", target.Queue, mapped.Queue)
			}
			return RunnerTarget{}, fmt.Errorf("runner labels resolve to conflicting targets %q and %q", targetDescription(target), targetDescription(mapped))
		}
		target = mapped
		resolved = true
	}
	if trust == EventUntrusted && target.Queue == "" && !policy.AllowUntrustedDefaultQueue {
		return RunnerTarget{}, fmt.Errorf("untrusted event cannot use Buildkite default agent targeting")
	}
	if trust == EventUntrusted && target.Queue != "" && !contains(policy.UntrustedQueues, target.Queue) {
		allowlist := append([]string(nil), policy.UntrustedQueues...)
		sort.Strings(allowlist)
		return RunnerTarget{}, fmt.Errorf("untrusted event cannot target queue %q; allowed queues: %s", target.Queue, strings.Join(allowlist, ", "))
	}
	return target, nil
}

func unsupportedOS(label string) bool {
	return strings.HasPrefix(label, "windows-") || label == "windows"
}

func targetDescription(target RunnerTarget) string {
	queue := target.Queue
	if queue == "" {
		queue = "<default>"
	}
	description := queue + "@" + target.Platform.String()
	if target.Image != "" {
		description += "#" + target.Image
	}
	return description
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
