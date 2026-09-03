package compiler

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

var queuePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var distributionDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var runtimeImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)
var stepKeyNamespacePattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

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

// CacheVolume is one Buildkite Hosted cache volume attached to a runner target.
type CacheVolume = buildkitepipeline.CacheVolume

// RunnerTarget atomically selects one Buildkite queue, execution platform, and
// optional immutable runtime image and cache volume.
type RunnerTarget struct {
	Queue    string
	Platform Platform
	Image    string
	Cache    *CacheVolume
}

// RunnerSelector maps one complete runs-on selector to a target without
// changing how any of its individual labels resolve in other selectors.
type RunnerSelector struct {
	Labels []string
	Target RunnerTarget
}

// RunnerRejection records that the Buildkite Agent API refused one complete
// runs-on selector. Code is server-owned and open-ended; Message is the
// server's explanation and never quotes runner labels.
type RunnerRejection struct {
	Labels  []string
	Code    string
	Message string
}

// Agent API rejection codes the compiler renders with dedicated guidance.
// Unknown codes render the server message with generic mapping guidance.
const (
	RunnerRejectionIncompatibleLabels = "incompatible_labels"
	RunnerRejectionMissingQueue       = "missing_queue"
	RunnerRejectionNoCluster          = "no_cluster"
	RunnerRejectionUnmappedLabels     = "unmapped_labels"
)

// RunnerPolicy maps every accepted runner label to a Buildkite queue and
// execution platform. An empty Linux queue uses Buildkite's default agent
// targeting. Darwin always requires an explicit queue. Multi-label targets are
// resolved by Selectors first, refused by Rejections second, then accepted
// only when every label maps to the same complete target. A Rejection wins
// over per-label presets so a server-side cause such as a missing hosted
// queue is reported instead of a target that cannot be scheduled. Untrusted
// events are additionally restricted to UntrustedQueues unless the default is
// explicitly allowed.
type RunnerPolicy struct {
	// Labels retains the Linux/amd64 label-to-queue shorthand used by direct
	// compiler callers. New multi-platform policy should use Targets.
	Labels                     map[string]string
	Targets                    map[string]RunnerTarget
	Selectors                  []RunnerSelector
	Rejections                 []RunnerRejection
	UntrustedQueues            []string
	AllowUntrustedDefaultQueue bool
}

// Options are the complete non-secret graph-construction inputs beyond the
// workflow and canonical event snapshot.
type Options struct {
	Vars       VariableSources
	Runners    RunnerPolicy
	EventTrust EventTrust
	OIDC       *plan.OIDCConfiguration
	// RepositorySource resolves public repositories used by static remote
	// reusable-workflow calls. Callers should share one memoized source for the
	// complete validate, compile, and upload operation.
	RepositorySource RepositorySource
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
	// StepKeyNamespace deterministically separates generated keys when several
	// workflows are compiled into one Buildkite pipeline. Empty preserves the
	// legacy single-workflow keys.
	StepKeyNamespace string
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

// DefaultOptions returns the options used by convenience compiler entry
// points. Callers may add a RepositorySource before compilation.
func DefaultOptions() Options { return defaultOptions() }

func (options Options) validate() error {
	if options.EventTrust != EventTrusted && options.EventTrust != EventUntrusted {
		return fmt.Errorf("event trust must be %q or %q", EventTrusted, EventUntrusted)
	}
	if !options.ResolveActions && options.ActionSource != nil {
		return fmt.Errorf("action source configuration requires ResolveActions")
	}
	if options.StepKeyNamespace != "" && !stepKeyNamespacePattern.MatchString(options.StepKeyNamespace) {
		return fmt.Errorf("step key namespace %q must be 16 lowercase hexadecimal characters", options.StepKeyNamespace)
	}
	if len(options.Runners.Labels) == 0 && len(options.Runners.Targets) == 0 && len(options.Runners.Selectors) == 0 {
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
	selectors := make(map[string]RunnerTarget, len(options.Runners.Selectors))
	for _, selector := range options.Runners.Selectors {
		key, err := runnerSelectorKey(selector.Labels)
		if err != nil {
			return err
		}
		if err := validateRunnerTarget(make(map[string]RunnerTarget), strings.Join(selector.Labels, ", "), selector.Target); err != nil {
			return err
		}
		if existing, ok := selectors[key]; ok && !RunnerTargetsEqual(existing, selector.Target) {
			return fmt.Errorf("runner selector has conflicting target mappings")
		}
		selectors[key] = selector.Target
	}
	for _, rejection := range options.Runners.Rejections {
		key, err := runnerSelectorKey(rejection.Labels)
		if err != nil {
			return err
		}
		if _, ok := selectors[key]; ok {
			return fmt.Errorf("runner selector is both resolved and rejected")
		}
		if strings.TrimSpace(rejection.Code) == "" || strings.TrimSpace(rejection.Message) == "" {
			return fmt.Errorf("runner rejection requires a code and message")
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
	if target.Cache != nil {
		if err := buildkitepipeline.ValidateCacheVolume(*target.Cache); err != nil {
			return fmt.Errorf("runner label %q has invalid cache configuration: %w", label, err)
		}
	}
	normalized := strings.ToLower(strings.TrimSpace(label))
	if existing, ok := labels[normalized]; ok && !RunnerTargetsEqual(existing, target) {
		return fmt.Errorf("runner label %q has conflicting target mappings", label)
	}
	labels[normalized] = target
	return nil
}

// RunnerTargetsEqual reports whether two complete runner mappings are equivalent.
func RunnerTargetsEqual(first, second RunnerTarget) bool {
	if first.Queue != second.Queue || first.Platform != second.Platform || first.Image != second.Image {
		return false
	}
	return cachesEqual(first.Cache, second.Cache)
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

// Every runs-on rejection reason a processing report may render. Resolved
// labels can interpolate event payload data, so a reason must stay a literal
// and never interpolate a resolved value.
const (
	reasonNoLabels          = "runs-on must resolve to at least one label"
	reasonDuplicateLabel    = "duplicate runner label"
	reasonUnsupportedOS     = "unsupported operating system"
	reasonUnmappedLabel     = "runner label is not mapped by policy"
	reasonConflictingQueues = "labels resolve to conflicting queues"
	reasonConflictingTarget = "labels resolve to conflicting targets"
	reasonUntrustedDefault  = "untrusted event cannot use Buildkite default agent targeting"
	reasonUntrustedQueue    = "untrusted event cannot target the resolved queue"
	reasonServerRejected    = "runner selector was rejected by the Buildkite Agent API"
)

// runnerPolicyRejection pairs a rejected runs-on resolution with its reason.
// Reports may render reason but never the detailed error, which quotes the
// resolved label. server is set when the Agent API refused the selector; its
// message is server-authored and safe to render.
type runnerPolicyRejection struct {
	reason string
	label  string
	server *RunnerRejection
	err    error
}

func (e *runnerPolicyRejection) Error() string { return e.err.Error() }
func (e *runnerPolicyRejection) Unwrap() error { return e.err }

func rejectRunner(reason, format string, args ...any) error {
	return &runnerPolicyRejection{reason: reason, err: fmt.Errorf(format, args...)}
}

func rejectRunnerLabel(reason, label, format string, args ...any) error {
	return &runnerPolicyRejection{reason: reason, label: label, err: fmt.Errorf(format, args...)}
}

func rejectRunnerByServer(rejection RunnerRejection) error {
	return &runnerPolicyRejection{
		reason: reasonServerRejected, server: &rejection,
		err: fmt.Errorf("runner selector %q was rejected by the Buildkite Agent API (%s): %s", strings.Join(rejection.Labels, ", "), rejection.Code, rejection.Message),
	}
}

// Resolve returns the target selected by labels under this policy and trust
// boundary.
func (policy RunnerPolicy) Resolve(labels []string, trust EventTrust) (RunnerTarget, error) {
	return policy.resolve(labels, trust)
}

func (policy RunnerPolicy) resolve(labels []string, trust EventTrust) (RunnerTarget, error) {
	if len(labels) == 0 {
		return RunnerTarget{}, rejectRunner(reasonNoLabels, reasonNoLabels)
	}
	normalizedLabels := make([]string, len(labels))
	seen := make(map[string]struct{}, len(labels))
	for i, label := range labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if _, duplicate := seen[normalized]; duplicate {
			return RunnerTarget{}, rejectRunnerLabel(reasonDuplicateLabel, label, "runs-on contains duplicate runner label %q", label)
		}
		seen[normalized] = struct{}{}
		normalizedLabels[i] = normalized
	}
	selectorKey, _ := runnerSelectorKey(normalizedLabels)
	for _, selector := range policy.Selectors {
		key, err := runnerSelectorKey(selector.Labels)
		if err == nil && key == selectorKey {
			return policy.enforceRunnerTrust(selector.Target, trust)
		}
	}
	for _, rejection := range policy.Rejections {
		key, err := runnerSelectorKey(rejection.Labels)
		if err != nil || key != selectorKey {
			continue
		}
		// The local Windows guidance is more specific than a server
		// incompatibility message, so let the per-label loop report it.
		if !slices.ContainsFunc(normalizedLabels, unsupportedOS) {
			return RunnerTarget{}, rejectRunnerByServer(rejection)
		}
	}
	var target RunnerTarget
	resolved := false
	for i, label := range labels {
		normalized := normalizedLabels[i]
		if unsupportedOS(normalized) {
			return RunnerTarget{}, rejectRunnerLabel(reasonUnsupportedOS, label, "unsupported operating system runner label %q", label)
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
			return RunnerTarget{}, rejectRunnerLabel(reasonUnmappedLabel, label, "runner label %q is not mapped by policy", label)
		}
		if resolved && !RunnerTargetsEqual(target, mapped) {
			if target.Queue != mapped.Queue && target.Platform == mapped.Platform && target.Image == mapped.Image && cachesEqual(target.Cache, mapped.Cache) {
				return RunnerTarget{}, rejectRunner(reasonConflictingQueues, "runner labels resolve to conflicting queues %q and %q", target.Queue, mapped.Queue)
			}
			return RunnerTarget{}, rejectRunner(reasonConflictingTarget, "runner labels resolve to conflicting targets %q and %q", targetDescription(target), targetDescription(mapped))
		}
		target = mapped
		resolved = true
	}
	return policy.enforceRunnerTrust(target, trust)
}

func (policy RunnerPolicy) enforceRunnerTrust(target RunnerTarget, trust EventTrust) (RunnerTarget, error) {
	if trust == EventUntrusted && target.Queue == "" && !policy.AllowUntrustedDefaultQueue {
		return RunnerTarget{}, rejectRunner(reasonUntrustedDefault, reasonUntrustedDefault)
	}
	if trust == EventUntrusted && target.Queue != "" && !contains(policy.UntrustedQueues, target.Queue) {
		allowlist := append([]string(nil), policy.UntrustedQueues...)
		sort.Strings(allowlist)
		return RunnerTarget{}, rejectRunner(reasonUntrustedQueue, "untrusted event cannot target queue %q; allowed queues: %s", target.Queue, strings.Join(allowlist, ", "))
	}
	return target, nil
}

func runnerSelectorKey(labels []string) (string, error) {
	if len(labels) == 0 {
		return "", fmt.Errorf("runner selector must contain at least one label")
	}
	normalized := make([]string, len(labels))
	seen := make(map[string]bool, len(labels))
	for i, label := range labels {
		normalized[i] = strings.ToLower(strings.TrimSpace(label))
		if normalized[i] == "" {
			return "", fmt.Errorf("runner selector labels must be non-empty")
		}
		if seen[normalized[i]] {
			return "", fmt.Errorf("runner selector contains duplicate label %q", label)
		}
		seen[normalized[i]] = true
	}
	sort.Strings(normalized)
	return strings.Join(normalized, "\x00"), nil
}

// runnerRejectionDiagnostic renders a rejected runs-on resolution. Callers
// pass labels only when they came from workflow-authored static data.
func runnerRejectionDiagnostic(err error, labels, supported, untrustedQueues []string) (message, detail string) {
	var rejection *runnerPolicyRejection
	if !errors.As(err, &rejection) {
		return "Runner target is unsupported. Use a configured Linux or macOS runner target.", ""
	}
	label := ""
	if len(labels) == 1 {
		label = fmt.Sprintf(" %q", labels[0])
	}
	if len(supported) != 0 {
		detail = "Supported runner labels: " + strings.Join(supported, ", ") + "."
	}
	switch rejection.reason {
	case reasonNoLabels:
		return "runs-on resolves to no runner labels. Set runs-on to a mapped Linux or macOS runner label.", detail
	case reasonDuplicateLabel:
		return "runs-on contains a duplicate runner label. Remove duplicate labels from runs-on.", ""
	case reasonUnsupportedOS:
		linuxGuidance := `If this job can run on Linux, change runs-on to "ubuntu-latest".`
		if label != "" {
			linuxGuidance = fmt.Sprintf(`If this job can run on Linux, change%s to "ubuntu-latest".`, label)
		}
		return "Windows runners aren't currently supported. Imported jobs run on Linux or macOS Buildkite hosted agents. " + linuxGuidance + " If it requires Windows, open an issue in https://github.com/buildkite/buildkite-gha to help us prioritize Windows support.", ""
	case reasonUnmappedLabel:
		return fmt.Sprintf("Runner label%s has no runner-target mapping. Configure a mapping for this label or use a mapped runner label.", label), detail
	case reasonServerRejected:
		if rejection.server != nil {
			return serverRunnerRejectionDiagnostic(*rejection.server, label, detail)
		}
		return "Runner target is unsupported. Use a configured Linux or macOS runner target.", detail
	case reasonConflictingQueues, reasonConflictingTarget:
		return "runs-on labels map to conflicting runner targets. Use labels that map to one runner target.", detail
	case reasonUntrustedDefault:
		return "This untrusted event cannot use the default runner. Use a runner label allowed for untrusted events, or ask an administrator to configure one.", detail
	case reasonUntrustedQueue:
		queues := append([]string(nil), untrustedQueues...)
		sort.Strings(queues)
		if len(queues) != 0 {
			detail = "Queues allowed for untrusted events: " + strings.Join(queues, ", ") + "."
		}
		return "This untrusted event uses a runner that is not allowed for untrusted events. Use an allowed runner label, or ask an administrator to allow its queue.", detail
	default:
		return "Runner target is unsupported. Use a configured Linux or macOS runner target.", detail
	}
}

// serverRunnerRejectionDiagnostic renders an Agent API rejection. The server
// message is rendered verbatim because it names the cause the workflow author
// cannot see locally, such as the cluster and the missing hosted queue. Codes
// whose message already carries a remedy add no local guidance; unknown codes
// keep the generic mapping guidance so newer servers degrade gracefully.
func serverRunnerRejectionDiagnostic(rejection RunnerRejection, label, supportedDetail string) (message, detail string) {
	subject := "the runs-on labels"
	if label != "" {
		subject = "runner label" + label
	}
	message = fmt.Sprintf("Buildkite could not resolve %s. ", subject)
	switch rejection.Code {
	case RunnerRejectionMissingQueue, RunnerRejectionNoCluster:
		// The server message may end with a documentation URL; leave it intact.
		return message + strings.Join(strings.Fields(rejection.Message), " "), ""
	case RunnerRejectionIncompatibleLabels:
		return message + sentence(rejection.Message) + " Change runs-on to a Linux or macOS runner label that Buildkite hosted agents support.", supportedDetail
	default:
		return message + sentence(rejection.Message) + " Configure a mapping for this selector or use a mapped runner label.", supportedDetail
	}
}

// sentence normalizes server prose so local guidance can follow it.
func sentence(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" || strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") {
		return text
	}
	return text + "."
}

func runnerRejectionBlockerDetail(err error, reportableLabels []string) string {
	var rejection *runnerPolicyRejection
	if errors.As(err, &rejection) && rejection.label != "" && slices.Contains(reportableLabels, rejection.label) {
		return rejection.label
	}
	if len(reportableLabels) == 1 {
		return reportableLabels[0]
	}
	return ""
}

func (policy RunnerPolicy) supportedLabels() []string {
	labels := make(map[string]bool, len(policy.Labels)+len(policy.Targets))
	for label := range policy.Labels {
		labels[label] = true
	}
	for label := range policy.Targets {
		labels[label] = true
	}
	return sortedKeys(labels)
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
	if target.Cache != nil {
		description += "#cache=" + target.Cache.Name + ":" + target.Cache.Size + ":" + strings.Join(target.Cache.Paths, ",")
	}
	return description
}

func cachesEqual(first, second *CacheVolume) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Name == second.Name && first.Size == second.Size && slices.Equal(first.Paths, second.Paths)
}

func contains(values []string, wanted string) bool {
	return slices.Contains(values, wanted)
}
