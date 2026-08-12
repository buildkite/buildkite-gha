// Package plan owns the versioned job-plan boundary between compilation and execution.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
)

const (
	SchemaV1 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v1.schema.json"
	SchemaV2 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v2.schema.json"
	SchemaV3 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v3.schema.json"
	SchemaV4 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v4.schema.json"
	SchemaV5 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v5.schema.json"
	SchemaV6 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v6.schema.json"
	SchemaV7 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v7.schema.json"
	SchemaV8 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v8.schema.json"
	Schema   = SchemaV2
)

const MaxNeedProducers = 1024
const maxLegacyNeedProducers = 256
const MaxNeedOutputs = 64
const maxStepTargets = 256

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var compilerVersionPattern = regexp.MustCompile(`^[ -~]{1,256}$`)
var targetPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var logicalJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var actionLockIDPattern = regexp.MustCompile(`^a-[0-9a-f]{16}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var containerImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[0-9a-f]{64})?$`)
var containerEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var containerPortPattern = regexp.MustCompile(`^(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])(?::(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?(?:/(?:tcp|udp))?$`)
var serviceNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,254}$`)
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

var githubTokenPermissionAccess = map[string]map[string]bool{
	"actions":             {"read": true, "write": true},
	"artifact_metadata":   {"read": true, "write": true},
	"attestations":        {"read": true, "write": true},
	"checks":              {"read": true, "write": true},
	"contents":            {"read": true, "write": true},
	"deployments":         {"read": true, "write": true},
	"discussions":         {"read": true, "write": true},
	"issues":              {"read": true, "write": true},
	"models":              {"read": true},
	"packages":            {"read": true, "write": true},
	"pages":               {"read": true, "write": true},
	"pull_requests":       {"read": true, "write": true},
	"repository_projects": {"read": true, "write": true},
	"security_events":     {"read": true, "write": true},
	"statuses":            {"read": true, "write": true},
}

type ActionSelector struct {
	Lock string `json:"lock"`
}

type ActionLock struct {
	ID           string                    `json:"id"`
	Source       string                    `json:"source"`
	Repository   string                    `json:"repository,omitempty"`
	RequestedRef string                    `json:"requested_ref,omitempty"`
	Commit       string                    `json:"commit,omitempty"`
	Path         string                    `json:"path,omitempty"`
	SourceDigest string                    `json:"source_digest"`
	Children     map[string]ActionSelector `json:"children,omitempty"`
}

type Compiler struct {
	Version            string `json:"version"`
	DistributionDigest string `json:"distribution_digest"`
}

// Runtime binds a job plan to the exact executable permitted to run it.
type Runtime struct {
	DistributionDigest string `json:"distribution_digest"`
}

type Event struct {
	Provider      string `json:"provider"`
	Name          string `json:"name"`
	PayloadDigest string `json:"payload_digest"`
	Repository    string `json:"repository,omitempty"`
	Ref           string `json:"ref,omitempty"`
	SHA           string `json:"sha,omitempty"`
	Actor         string `json:"actor,omitempty"`
}

type Workflow struct {
	Path         string `json:"path"`
	Digest       string `json:"digest"`
	LogicalJobID string `json:"logical_job_id"`
}

type Target struct {
	StepKey string `json:"step_key"`
	Queue   string `json:"queue,omitempty"`
}

type Need struct {
	Result    string            `json:"result"`
	Outputs   map[string]string `json:"outputs,omitempty"`
	Artifacts []NeedArtifact    `json:"-"`
}

// NeedArtifact is verified runtime-only native storage authority.
type NeedArtifact struct {
	Name, ID, Path, Digest string
	Size                   int64
	FileCount              int
	Producer               NeedProducer
}

type NeedProducer struct{ BuildID, JobID, StepKey string }

// NeedSource binds one logical prerequisite to an exact generated producer
// and immutable plan. Buildkite scheduling still uses Dependencies; runtimes
// use these identities to select and verify authoritative result artifacts.
type NeedSource struct {
	StepKey    string `json:"step_key"`
	PlanDigest string `json:"plan_digest"`
}

// NeedOutput projects one named producer output into a logical prerequisite.
// A NeedOutputs key with an empty list deliberately exposes no outputs.
type NeedOutput struct {
	Name    string `json:"name"`
	StepKey string `json:"step_key"`
	Output  string `json:"output"`
}

type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Step struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	Kind             string            `json:"kind"`
	Background       bool              `json:"background,omitempty"`
	Targets          []string          `json:"targets,omitempty"`
	Command          string            `json:"command,omitempty"`
	Uses             string            `json:"uses,omitempty"`
	Action           *ActionSelector   `json:"action,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	With             map[string]string `json:"with,omitempty"`
	Condition        string            `json:"condition,omitempty"`
	ContinueOnError  bool              `json:"continue_on_error,omitempty"`
	TimeoutMinutes   float64           `json:"timeout_minutes,omitempty"`
	Source           *Span             `json:"source,omitempty"`
}

type Container struct {
	Image string            `json:"image"`
	Env   map[string]string `json:"env,omitempty"`
	Ports []string          `json:"ports,omitempty"`
}

// GitHubToken describes one synthetic secrets.GITHUB_TOKEN value. Permissions
// are API-normalized and compiler-owned; the repository always comes from Event.
type GitHubToken struct {
	Permissions map[string]string `json:"permissions"`
}

// Job is one immutable, compiler-selected workflow job instance.
type Job struct {
	Schema               string                  `json:"schema"`
	Compiler             Compiler                `json:"compiler"`
	Runtime              *Runtime                `json:"runtime,omitempty"`
	Workflow             Workflow                `json:"workflow"`
	Event                Event                   `json:"event"`
	Target               Target                  `json:"target"`
	RequiredCapabilities []string                `json:"required_capabilities"`
	RequiredSecrets      []string                `json:"required_secrets,omitempty"`
	GitHubToken          *GitHubToken            `json:"github_token,omitempty"`
	Matrix               map[string]any          `json:"matrix,omitempty"`
	Vars                 map[string]string       `json:"vars,omitempty"`
	Dependencies         []string                `json:"dependencies,omitempty"`
	NeedSources          map[string][]NeedSource `json:"need_sources,omitempty"`
	NeedOutputs          map[string][]NeedOutput `json:"need_outputs,omitempty"`
	// Needs is populated only from verified producer-attributed manifests at
	// runtime. It is never accepted from or encoded into an immutable plan.
	Needs                   map[string]Need   `json:"-"`
	Env                     map[string]string `json:"env,omitempty"`
	Condition               string            `json:"condition,omitempty"`
	TimeoutMinutes          float64           `json:"timeout_minutes,omitempty"`
	DefaultShell            string            `json:"default_shell,omitempty"`
	DefaultWorkingDirectory string            `json:"default_working_directory,omitempty"`
	Outputs                 map[string]string `json:"outputs,omitempty"`
	Steps                   []Step            `json:"steps"`
	Actions                 []ActionLock      `json:"actions,omitempty"`
	// RequiresMise is compiler-resolved action runtime metadata. Nil preserves
	// fail-closed behavior for older and unresolved action plans.
	RequiresMise *bool                `json:"requires_mise,omitempty"`
	Container    *Container           `json:"container,omitempty"`
	Services     map[string]Container `json:"services,omitempty"`
}

// NeedsMise reports whether a generated job needs the managed action runtime.
func (job Job) NeedsMise() bool {
	usesActions := len(job.Actions) != 0
	for _, step := range job.Steps {
		usesActions = usesActions || step.Uses != "" || step.Action != nil || step.Kind == "uses"
	}
	if !usesActions {
		return false
	}
	return job.RequiresMise == nil || *job.RequiresMise
}

// RuntimeDistributionDigest returns the executable digest bound to this plan.
// Plans before v8 used the compiler executable as their runtime executable.
func (job Job) RuntimeDistributionDigest() string {
	if job.Schema == SchemaV8 && job.Runtime != nil {
		return job.Runtime.DistributionDigest
	}
	return job.Compiler.DistributionDigest
}

// Decode rejects unknown fields and trailing JSON so schema drift fails closed.
func Decode(source []byte) (Job, error) {
	if err := rejectDuplicateKeys(source); err != nil {
		return Job{}, fmt.Errorf("decode job plan: %w", err)
	}
	if err := rejectLegacyActionFields(source); err != nil {
		return Job{}, fmt.Errorf("decode job plan: %w", err)
	}
	var job Job
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&job); err != nil {
		return Job{}, fmt.Errorf("decode job plan: %w", err)
	}
	if job.RequiredCapabilities == nil {
		return Job{}, fmt.Errorf("decode job plan: required_capabilities must be a concrete array")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Job{}, fmt.Errorf("decode job plan: multiple JSON values")
		}
		return Job{}, fmt.Errorf("decode job plan: %w", err)
	}
	if err := job.Validate(); err != nil {
		return Job{}, err
	}
	return job, nil
}

func rejectLegacyActionFields(data []byte) error {
	var envelope struct {
		Schema string                       `json:"schema"`
		Steps  []map[string]json.RawMessage `json:"steps"`
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(data, &envelope) != nil || envelope.Schema != SchemaV1 && envelope.Schema != SchemaV2 {
		return nil
	}
	if _, ok := root["actions"]; ok {
		return fmt.Errorf("field actions is not supported by legacy schemas")
	}
	for _, step := range envelope.Steps {
		if _, ok := step["action"]; ok {
			return fmt.Errorf("field action is not supported by legacy schemas")
		}
	}
	return nil
}

func rejectDuplicateKeys(source []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var check func() error
	check = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = struct{}{}
				if err := check(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := check(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
		_, err = decoder.Token()
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

// Encode returns stable indented JSON terminated by a newline.
func Encode(job Job) ([]byte, error) {
	if job.RequiredCapabilities == nil {
		job.RequiredCapabilities = []string{}
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(job); err != nil {
		return nil, fmt.Errorf("encode job plan: %w", err)
	}
	return out.Bytes(), nil
}

func (job Job) Validate() error {
	if job.Schema != SchemaV1 && job.Schema != SchemaV2 && job.Schema != SchemaV3 && job.Schema != SchemaV4 && job.Schema != SchemaV5 && job.Schema != SchemaV6 && job.Schema != SchemaV7 && job.Schema != SchemaV8 {
		return fmt.Errorf("unsupported job plan schema %q", job.Schema)
	}
	if job.Schema != SchemaV3 && job.Schema != SchemaV4 && job.Schema != SchemaV5 && job.Schema != SchemaV6 && job.Schema != SchemaV7 && job.Schema != SchemaV8 && (len(job.Actions) != 0 || hasStepActions(job.Steps)) {
		return fmt.Errorf("job plan %s does not support action locks", job.Schema)
	}
	if job.Schema != SchemaV4 && job.Schema != SchemaV5 && job.Schema != SchemaV6 && job.Schema != SchemaV7 && job.Schema != SchemaV8 && (job.Container != nil || len(job.Services) != 0) {
		return fmt.Errorf("job plan %s does not support containers or services", job.Schema)
	}
	if job.Schema != SchemaV5 && job.Schema != SchemaV6 && job.Schema != SchemaV7 && job.Schema != SchemaV8 && job.NeedOutputs != nil {
		return fmt.Errorf("job plan %s does not support prerequisite output projections", job.Schema)
	}
	if job.Schema != SchemaV6 && job.Schema != SchemaV7 && job.Schema != SchemaV8 && job.GitHubToken != nil {
		return fmt.Errorf("job plan %s does not support GitHub workflow tokens", job.Schema)
	}
	if (job.Schema == SchemaV7 || job.Schema == SchemaV8) && job.RequiresMise == nil {
		return fmt.Errorf("job plan %s requires an explicit requires_mise decision", job.Schema)
	}
	if job.Schema != SchemaV7 && job.Schema != SchemaV8 && job.RequiresMise != nil {
		return fmt.Errorf("job plan %s does not support requires_mise", job.Schema)
	}
	if job.Schema == SchemaV8 {
		if job.Runtime == nil || !digestPattern.MatchString(job.Runtime.DistributionDigest) {
			return fmt.Errorf("job plan runtime distribution digest is required")
		}
	} else if job.Runtime != nil {
		return fmt.Errorf("job plan %s does not support a separate runtime distribution", job.Schema)
	}
	if job.RequiresMise != nil && !*job.RequiresMise {
		for _, step := range job.Steps {
			if (step.Kind == "uses" || step.Uses != "") && step.Action == nil {
				return fmt.Errorf("job plan requires_mise may be false only when every action has an immutable selector")
			}
		}
	}
	if !compilerVersionPattern.MatchString(job.Compiler.Version) || !digestPattern.MatchString(job.Compiler.DistributionDigest) {
		return fmt.Errorf("job plan compiler version and distribution digest are required")
	}
	if (job.Event.Provider != "github" && job.Event.Provider != "cursor-origin") || job.Event.Name == "" || !digestPattern.MatchString(job.Event.PayloadDigest) {
		return fmt.Errorf("job plan requires a supported event binding")
	}
	if len(job.Event.Repository) > 512 || len(job.Event.Ref) > 1024 || len(job.Event.SHA) > 128 || len(job.Event.Actor) > 256 {
		return fmt.Errorf("job plan event identity exceeds its size limit")
	}
	if job.Workflow.Path == "" || !digestPattern.MatchString(job.Workflow.Digest) || job.Workflow.LogicalJobID == "" {
		return fmt.Errorf("job plan requires a workflow path, sha256 digest, and logical job id")
	}
	if !targetPattern.MatchString(job.Target.StepKey) {
		return fmt.Errorf("job plan requires a target step key")
	}
	if job.Schema != SchemaV7 && job.Schema != SchemaV8 && !targetPattern.MatchString(job.Target.Queue) {
		return fmt.Errorf("job plan %s requires a target queue", job.Schema)
	}
	if job.Target.Queue != "" && !targetPattern.MatchString(job.Target.Queue) {
		return fmt.Errorf("job plan has invalid target queue %q", job.Target.Queue)
	}
	if job.TimeoutMinutes < 0 || job.TimeoutMinutes > 360 {
		return fmt.Errorf("job timeout_minutes must be between 0 and 360")
	}
	if len(job.Condition) > 65536 || len(job.RequiredSecrets) > 128 {
		return fmt.Errorf("job plan condition or required secrets exceed their size limit")
	}
	capabilities := make(map[string]struct{}, len(job.RequiredCapabilities))
	if !sort.StringsAreSorted(job.RequiredCapabilities) {
		return fmt.Errorf("job plan capabilities must be sorted")
	}
	for _, capability := range job.RequiredCapabilities {
		switch capability {
		case "network", "docker", "privileged-container", "secrets", "provider-token-read", "provider-token-write":
		default:
			return fmt.Errorf("job plan requires unsupported capability %q", capability)
		}
		if _, exists := capabilities[capability]; exists {
			return fmt.Errorf("job plan repeats capability %q", capability)
		}
		capabilities[capability] = struct{}{}
	}
	if job.Container != nil || len(job.Services) != 0 {
		if _, ok := capabilities["docker"]; !ok {
			return fmt.Errorf("job containers and services require docker capability")
		}
		if _, ok := capabilities["network"]; !ok {
			return fmt.Errorf("job containers and services require network capability")
		}
		if job.Container != nil {
			if err := validateContainer(job.Container.Image, job.Container.Env, job.Container.Ports); err != nil {
				return fmt.Errorf("job container: %w", err)
			}
		}
		if len(job.Services) > 32 {
			return fmt.Errorf("job plan has more than 32 services")
		}
		for name, service := range job.Services {
			if !serviceNamePattern.MatchString(name) {
				return fmt.Errorf("service name %q must be lowercase and valid", name)
			}
			if err := validateContainer(service.Image, service.Env, service.Ports); err != nil {
				return fmt.Errorf("service %q: %w", name, err)
			}
		}
	}
	if !sort.StringsAreSorted(job.RequiredSecrets) {
		return fmt.Errorf("job plan required secrets must be sorted")
	}
	for i, name := range job.RequiredSecrets {
		if !secretNamePattern.MatchString(name) || i > 0 && job.RequiredSecrets[i-1] == name {
			return fmt.Errorf("job plan contains invalid or repeated required secret %q", name)
		}
		if name == "GITHUB_TOKEN" {
			return fmt.Errorf("job plan must provide GITHUB_TOKEN through the scoped workflow token contract")
		}
	}
	if len(job.RequiredSecrets) != 0 {
		if _, ok := capabilities["secrets"]; !ok {
			return fmt.Errorf("job plan required secrets need the secrets capability")
		}
	}
	_, workflowTokenCapability := capabilities["provider-token-write"]
	if (job.GitHubToken != nil) != workflowTokenCapability {
		return fmt.Errorf("job plan GitHub workflow token and provider-token-write capability must be declared together")
	}
	if job.GitHubToken != nil {
		if job.Event.Provider != "github" || !validGitHubRepository(job.Event.Repository) {
			return fmt.Errorf("GitHub workflow token requires a valid github.com event repository")
		}
		if err := validateGitHubTokenPermissions(job.GitHubToken.Permissions); err != nil {
			return err
		}
	}
	if !sort.StringsAreSorted(job.Dependencies) {
		return fmt.Errorf("job plan dependencies must be sorted")
	}
	dependencies := make(map[string]struct{}, len(job.Dependencies))
	for _, dependency := range job.Dependencies {
		if !targetPattern.MatchString(dependency) || strings.EqualFold(dependency, job.Target.StepKey) {
			return fmt.Errorf("job plan contains invalid dependency %q", dependency)
		}
		id := strings.ToLower(dependency)
		if _, exists := dependencies[id]; exists {
			return fmt.Errorf("job plan repeats dependency %q", dependency)
		}
		dependencies[id] = struct{}{}
	}
	needIDs := make(map[string]struct{}, len(job.NeedSources))
	sourcedDependencies := make(map[string]struct{}, len(job.Dependencies))
	maxNeedProducers := MaxNeedProducers
	if job.Schema != SchemaV5 && job.Schema != SchemaV6 && job.Schema != SchemaV7 && job.Schema != SchemaV8 {
		maxNeedProducers = maxLegacyNeedProducers
	}
	for name, sources := range job.NeedSources {
		if len(name) > 255 || !logicalJobIDPattern.MatchString(name) || len(sources) == 0 || len(sources) > maxNeedProducers {
			return fmt.Errorf("job plan contains invalid prerequisite %q", name)
		}
		id := strings.ToLower(name)
		if _, exists := needIDs[id]; exists {
			return fmt.Errorf("job plan contains duplicate prerequisite %q", name)
		}
		needIDs[id] = struct{}{}
		for i, source := range sources {
			if !targetPattern.MatchString(source.StepKey) || !digestPattern.MatchString(source.PlanDigest) {
				return fmt.Errorf("job plan prerequisite %q has invalid producer identity", name)
			}
			if i > 0 && sources[i-1].StepKey >= source.StepKey {
				return fmt.Errorf("job plan prerequisite %q producers must be unique and sorted", name)
			}
			key := strings.ToLower(source.StepKey)
			if _, exists := dependencies[key]; !exists {
				return fmt.Errorf("job plan prerequisite %q producer %q is not a dependency", name, source.StepKey)
			}
			if _, exists := sourcedDependencies[key]; exists {
				return fmt.Errorf("job plan dependency %q has multiple logical owners", source.StepKey)
			}
			sourcedDependencies[key] = struct{}{}
		}
	}
	if len(sourcedDependencies) != len(dependencies) {
		return fmt.Errorf("job plan dependencies and prerequisite producers differ")
	}
	needSourcesByName := make(map[string]map[string]struct{}, len(job.NeedSources))
	for name, sources := range job.NeedSources {
		steps := make(map[string]struct{}, len(sources))
		for _, source := range sources {
			steps[strings.ToLower(source.StepKey)] = struct{}{}
		}
		needSourcesByName[strings.ToLower(name)] = steps
	}
	projectedNeeds := make(map[string]struct{}, len(job.NeedOutputs))
	for name, outputs := range job.NeedOutputs {
		lowerName := strings.ToLower(name)
		if _, exists := projectedNeeds[lowerName]; exists {
			return fmt.Errorf("job plan contains duplicate prerequisite output projection %q", name)
		}
		projectedNeeds[lowerName] = struct{}{}
		producers, exists := needSourcesByName[lowerName]
		if !exists {
			return fmt.Errorf("job plan prerequisite output projection %q has no matching prerequisite", name)
		}
		if len(outputs) > MaxNeedOutputs {
			return fmt.Errorf("job plan prerequisite %q has more than %d projected outputs", name, MaxNeedOutputs)
		}
		seen := make(map[string]struct{}, len(outputs))
		for i, output := range outputs {
			if !targetPattern.MatchString(output.Name) || !targetPattern.MatchString(output.Output) || !targetPattern.MatchString(output.StepKey) {
				return fmt.Errorf("job plan prerequisite %q has invalid output projection", name)
			}
			if _, exists := producers[strings.ToLower(output.StepKey)]; !exists {
				return fmt.Errorf("job plan prerequisite %q output %q selects unknown producer %q", name, output.Name, output.StepKey)
			}
			if i > 0 && compareNeedOutput(outputs[i-1], output) >= 0 {
				return fmt.Errorf("job plan prerequisite %q output projections must be unique and sorted", name)
			}
			key := strings.ToLower(output.Name) + "\x00" + strings.ToLower(output.StepKey)
			if _, exists := seen[key]; exists {
				return fmt.Errorf("job plan prerequisite %q repeats output projection %q from %q", name, output.Name, output.StepKey)
			}
			seen[key] = struct{}{}
		}
	}
	if len(job.Steps) == 0 {
		return fmt.Errorf("job plan contains no steps")
	}
	ids := make(map[string]struct{}, len(job.Steps))
	backgroundIDs := make(map[string]struct{})
	for i, step := range job.Steps {
		if step.ID == "" {
			return fmt.Errorf("job plan step %d has no deterministic id", i+1)
		}
		if len(step.ID) > 255 {
			return fmt.Errorf("job plan step id %q exceeds 255 bytes", step.ID)
		}
		id := strings.ToLower(step.ID)
		if _, exists := ids[id]; exists {
			return fmt.Errorf("job plan contains duplicate step id %q", step.ID)
		}
		ids[id] = struct{}{}
		if step.TimeoutMinutes < 0 || step.TimeoutMinutes > 360 {
			return fmt.Errorf("job plan step %q timeout_minutes must be between 0 and 360", step.ID)
		}
		if len(step.Condition) > 65536 {
			return fmt.Errorf("job plan step %q condition exceeds 65536 bytes", step.ID)
		}
		if job.Schema == SchemaV1 && (step.Background || len(step.Targets) != 0 || step.Kind != "run" && step.Kind != "uses") {
			return fmt.Errorf("job plan v1 step %q contains concurrent-step fields", step.ID)
		}
		switch step.Kind {
		case "run":
			if strings.TrimSpace(step.Command) == "" {
				return fmt.Errorf("run step %q has no command", step.ID)
			}
			if step.Uses != "" || step.Action != nil || len(step.Targets) != 0 {
				return fmt.Errorf("run step %q contains incompatible action or control fields", step.ID)
			}
			if step.Background {
				backgroundIDs[id] = struct{}{}
			}
		case "uses":
			if strings.TrimSpace(step.Uses) == "" {
				return fmt.Errorf("action step %q has no action reference", step.ID)
			}
			if len(step.Uses) > 1024 {
				return fmt.Errorf("action step %q reference exceeds 1024 bytes", step.ID)
			}
			if step.Command != "" || len(step.Targets) != 0 {
				return fmt.Errorf("action step %q contains incompatible run or control fields", step.ID)
			}
			if step.Background {
				backgroundIDs[id] = struct{}{}
			}
		case "wait", "wait-all", "cancel":
			if err := validateControlStep(step, backgroundIDs); err != nil {
				return err
			}
		default:
			return fmt.Errorf("step %q has unsupported kind %q", step.ID, step.Kind)
		}
	}
	if job.Schema == SchemaV3 || job.Schema == SchemaV4 || ((job.Schema == SchemaV5 || job.Schema == SchemaV6 || job.Schema == SchemaV7 || job.Schema == SchemaV8) && (len(job.Actions) != 0 || hasStepActions(job.Steps))) {
		if err := validateActionLocks(job); err != nil {
			return err
		}
	}
	return nil
}

func validGitHubRepository(repository string) bool {
	if len(repository) > 140 || !githubRepositoryPattern.MatchString(repository) {
		return false
	}
	parts := strings.Split(repository, "/")
	return parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".."
}

func validateGitHubTokenPermissions(permissions map[string]string) error {
	if len(permissions) == 0 || len(permissions) > len(githubTokenPermissionAccess) {
		return fmt.Errorf("GitHub workflow token requires a non-empty bounded permission set")
	}
	for name, access := range permissions {
		allowed, ok := githubTokenPermissionAccess[name]
		if !ok || !allowed[access] {
			return fmt.Errorf("GitHub workflow token contains unsupported permission %q with access %q", name, access)
		}
	}
	return nil
}

func compareNeedOutput(left, right NeedOutput) int {
	if left.Name != right.Name {
		return strings.Compare(left.Name, right.Name)
	}
	if left.StepKey != right.StepKey {
		return strings.Compare(left.StepKey, right.StepKey)
	}
	return strings.Compare(left.Output, right.Output)
}

func validateContainer(image string, env map[string]string, ports []string) error {
	if len(image) == 0 || len(image) > 512 || !containerImagePattern.MatchString(image) {
		return fmt.Errorf("invalid image reference")
	}
	if len(env) > 256 {
		return fmt.Errorf("environment has more than 256 entries")
	}
	total := 0
	for key, value := range env {
		if !containerEnvKeyPattern.MatchString(key) || len(key) > 255 || len(value) > 65536 {
			return fmt.Errorf("invalid environment entry %q", key)
		}
		total += len(key) + len(value)
	}
	if total > 1048576 {
		return fmt.Errorf("environment exceeds 1048576 bytes")
	}
	if len(ports) > 128 {
		return fmt.Errorf("more than 128 ports")
	}
	seen := map[string]bool{}
	for _, port := range ports {
		if seen[port] {
			return fmt.Errorf("repeated port %q", port)
		}
		seen[port] = true
		if !containerPortPattern.MatchString(port) {
			return fmt.Errorf("invalid port %q", port)
		}
	}
	return nil
}

func hasStepActions(steps []Step) bool {
	for _, step := range steps {
		if step.Action != nil {
			return true
		}
	}
	return false
}

func validateActionLocks(job Job) error {
	if len(job.Actions) > 1024 {
		return fmt.Errorf("job plan has more than 1024 action locks")
	}
	locks := make(map[string]ActionLock, len(job.Actions))
	for i, lock := range job.Actions {
		if !actionLockIDPattern.MatchString(lock.ID) || i > 0 && job.Actions[i-1].ID >= lock.ID {
			return fmt.Errorf("action locks must have valid, unique, sorted IDs")
		}
		if !digestPattern.MatchString(lock.SourceDigest) || len(lock.Children) > 1024 {
			return fmt.Errorf("action lock %q has invalid digest or too many children", lock.ID)
		}
		if err := validateLockIdentity(lock); err != nil {
			return fmt.Errorf("action lock %q: %w", lock.ID, err)
		}
		for uses, child := range lock.Children {
			if len(uses) == 0 || len(uses) > 2048 || !utf8.ValidString(uses) || hasControl(uses) || !actionLockIDPattern.MatchString(child.Lock) {
				return fmt.Errorf("action lock %q has invalid child selector", lock.ID)
			}
		}
		locks[lock.ID] = lock
	}
	reachable := map[string]bool{}
	state := map[string]uint8{}
	heights := map[string]int{}
	var visit func(string, int) (int, error)
	visit = func(id string, depth int) (int, error) {
		lock, ok := locks[id]
		if !ok {
			return 0, fmt.Errorf("action selector references missing lock %q", id)
		}
		if state[id] == 1 {
			return 0, fmt.Errorf("action lock graph contains a cycle at %q", id)
		}
		if height, ok := heights[id]; ok {
			if depth+height-1 > metadata.MaxNestedActionDepth {
				return 0, fmt.Errorf("action lock graph exceeds maximum depth %d", metadata.MaxNestedActionDepth)
			}
			reachable[id] = true
			return height, nil
		}
		if depth > metadata.MaxNestedActionDepth {
			return 0, fmt.Errorf("action lock graph exceeds maximum depth %d", metadata.MaxNestedActionDepth)
		}
		reachable[id] = true
		state[id] = 1
		height := 1
		for uses, selector := range lock.Children {
			child, ok := locks[selector.Lock]
			if !ok {
				return 0, fmt.Errorf("action selector references missing lock %q", selector.Lock)
			}
			if err := validateChildIdentity(lock, uses, child); err != nil {
				return 0, fmt.Errorf("action lock %q child %q: %w", id, uses, err)
			}
			childHeight, err := visit(selector.Lock, depth+1)
			if err != nil {
				return 0, err
			}
			if childHeight+1 > height {
				height = childHeight + 1
			}
		}
		state[id] = 0
		heights[id] = height
		return height, nil
	}
	for _, step := range job.Steps {
		if step.Kind != "uses" {
			if step.Action != nil {
				return fmt.Errorf("non-action step %q has an action selector", step.ID)
			}
			continue
		}
		if step.Action == nil {
			return fmt.Errorf("action step %q has no action selector", step.ID)
		}
		if !actionLockIDPattern.MatchString(step.Action.Lock) {
			return fmt.Errorf("action step %q has malformed action selector lock %q", step.ID, step.Action.Lock)
		}
		lock, ok := locks[step.Action.Lock]
		if !ok {
			return fmt.Errorf("action selector references missing lock %q", step.Action.Lock)
		}
		if err := validateTopLevelIdentity(step.Uses, lock); err != nil {
			return fmt.Errorf("action step %q: %w", step.ID, err)
		}
		if _, err := visit(lock.ID, 1); err != nil {
			return err
		}
	}
	if len(reachable) != len(locks) {
		return fmt.Errorf("job plan contains unused action locks")
	}
	return nil
}

func validateLockIdentity(lock ActionLock) error {
	switch lock.Source {
	case "workspace":
		if lock.Repository != "" || lock.RequestedRef != "" || lock.Commit != "" || lock.Path != "" && !cleanActionPath(lock.Path) {
			return fmt.Errorf("invalid workspace identity")
		}
	case "github":
		if lock.Repository == "" || len(lock.Repository) > 140 || lock.Repository != strings.ToLower(lock.Repository) || lock.RequestedRef == "" || len(lock.RequestedRef) > 1024 || !utf8.ValidString(lock.RequestedRef) || hasControl(lock.RequestedRef) || !commitPattern.MatchString(lock.Commit) || lock.Path != "" && !cleanActionPath(lock.Path) {
			return fmt.Errorf("invalid GitHub identity")
		}
		r, err := source.Parse(lock.Repository + "@x")
		if err != nil || r.Owner+"/"+r.Repository != lock.Repository {
			return fmt.Errorf("invalid canonical GitHub repository")
		}
	default:
		return fmt.Errorf("unsupported source %q", lock.Source)
	}
	return nil
}

func validateTopLevelIdentity(uses string, lock ActionLock) error {
	if strings.HasPrefix(uses, "./") {
		path := strings.TrimPrefix(uses, "./")
		if lock.Source != "workspace" || path != "" && !cleanActionPath(path) || lock.Path != path {
			return fmt.Errorf("local action reference does not match lock identity")
		}
		return nil
	}
	r, err := source.Parse(uses)
	if err != nil || lock.Source != "github" || !strings.EqualFold(lock.Repository, r.Owner+"/"+r.Repository) || lock.Path != r.Path || lock.RequestedRef != r.Ref {
		return fmt.Errorf("remote action reference does not match lock identity")
	}
	return nil
}

func validateChildIdentity(parent ActionLock, uses string, child ActionLock) error {
	if strings.HasPrefix(uses, "./") {
		path := strings.TrimPrefix(uses, "./")
		if path != "" && !cleanActionPath(path) || child.Source != "workspace" || child.Path != path {
			return fmt.Errorf("local child does not match workspace action identity")
		}
		return nil
	}
	r, err := source.Parse(uses)
	if err != nil || child.Source != "github" || !strings.EqualFold(child.Repository, r.Owner+"/"+r.Repository) || child.Path != r.Path || child.RequestedRef != r.Ref {
		return fmt.Errorf("remote child does not match lock identity")
	}
	return nil
}

func cleanActionPath(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || hasControl(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || len(segment) > 255 {
			return false
		}
	}
	return true
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateControlStep(step Step, backgroundIDs map[string]struct{}) error {
	if step.Background || step.Command != "" || step.Uses != "" || step.Action != nil || step.Shell != "" || step.WorkingDirectory != "" || len(step.Env) != 0 || len(step.With) != 0 || step.Condition != "" || step.ContinueOnError || step.TimeoutMinutes != 0 {
		return fmt.Errorf("control step %q contains incompatible execution fields", step.ID)
	}
	switch step.Kind {
	case "wait":
		if len(step.Targets) == 0 || len(step.Targets) > maxStepTargets {
			return fmt.Errorf("wait step %q must target between 1 and %d background steps", step.ID, maxStepTargets)
		}
	case "wait-all":
		if len(step.Targets) != 0 {
			return fmt.Errorf("wait-all step %q cannot target individual steps", step.ID)
		}
		return nil
	case "cancel":
		if len(step.Targets) != 1 {
			return fmt.Errorf("cancel step %q must target exactly one background step", step.ID)
		}
	}
	seen := make(map[string]struct{}, len(step.Targets))
	for _, target := range step.Targets {
		if !targetPattern.MatchString(target) {
			return fmt.Errorf("control step %q has invalid target %q", step.ID, target)
		}
		key := strings.ToLower(target)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("control step %q repeats target %q", step.ID, target)
		}
		seen[key] = struct{}{}
		if _, exists := backgroundIDs[key]; !exists {
			return fmt.Errorf("control step %q target %q is not a prior background step", step.ID, target)
		}
	}
	return nil
}

func (job Job) HasCapability(name string) bool {
	for _, capability := range job.RequiredCapabilities {
		if capability == name {
			return true
		}
	}
	return false
}
