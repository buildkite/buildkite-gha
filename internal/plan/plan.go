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
)

const (
	SchemaV1 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v1.schema.json"
	SchemaV2 = "https://buildkite.com/schemas/buildkite-gha/job-plan-v2.schema.json"
	Schema   = SchemaV2
)

const MaxNeedProducers = 256
const MaxStepTargets = 256

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var targetPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Compiler struct {
	Version            string `json:"version"`
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
	Queue   string `json:"queue"`
}

type Need struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs,omitempty"`
}

// NeedSource binds one logical prerequisite to an exact generated producer
// and immutable plan. Buildkite scheduling still uses Dependencies; runtimes
// use these identities to select and verify authoritative result artifacts.
type NeedSource struct {
	StepKey    string `json:"step_key"`
	PlanDigest string `json:"plan_digest"`
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
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	With             map[string]string `json:"with,omitempty"`
	Condition        string            `json:"condition,omitempty"`
	ContinueOnError  bool              `json:"continue_on_error,omitempty"`
	TimeoutMinutes   float64           `json:"timeout_minutes,omitempty"`
	Source           *Span             `json:"source,omitempty"`
}

// Job is one immutable, compiler-selected workflow job instance.
type Job struct {
	Schema               string                  `json:"schema"`
	Compiler             Compiler                `json:"compiler"`
	Workflow             Workflow                `json:"workflow"`
	Event                Event                   `json:"event"`
	Target               Target                  `json:"target"`
	RequiredCapabilities []string                `json:"required_capabilities"`
	RequiredSecrets      []string                `json:"required_secrets,omitempty"`
	Matrix               map[string]any          `json:"matrix,omitempty"`
	Vars                 map[string]string       `json:"vars,omitempty"`
	Dependencies         []string                `json:"dependencies,omitempty"`
	NeedSources          map[string][]NeedSource `json:"need_sources,omitempty"`
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
}

// Decode rejects unknown fields and trailing JSON so schema drift fails closed.
func Decode(source []byte) (Job, error) {
	if err := rejectDuplicateKeys(source); err != nil {
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
	if job.Schema != SchemaV1 && job.Schema != SchemaV2 {
		return fmt.Errorf("unsupported job plan schema %q", job.Schema)
	}
	if job.Compiler.Version == "" || !digestPattern.MatchString(job.Compiler.DistributionDigest) {
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
	if !targetPattern.MatchString(job.Target.StepKey) || !targetPattern.MatchString(job.Target.Queue) {
		return fmt.Errorf("job plan requires a target step key and queue")
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
	if !sort.StringsAreSorted(job.RequiredSecrets) {
		return fmt.Errorf("job plan required secrets must be sorted")
	}
	for i, name := range job.RequiredSecrets {
		if !secretNamePattern.MatchString(name) || i > 0 && job.RequiredSecrets[i-1] == name {
			return fmt.Errorf("job plan contains invalid or repeated required secret %q", name)
		}
	}
	if len(job.RequiredSecrets) != 0 {
		if _, ok := capabilities["secrets"]; !ok {
			return fmt.Errorf("job plan required secrets need the secrets capability")
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
	for name, sources := range job.NeedSources {
		if !targetPattern.MatchString(name) || len(sources) == 0 || len(sources) > MaxNeedProducers {
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
			if step.Uses != "" || len(step.Targets) != 0 {
				return fmt.Errorf("run step %q contains incompatible action or control fields", step.ID)
			}
			if step.Background {
				backgroundIDs[id] = struct{}{}
			}
		case "uses":
			if strings.TrimSpace(step.Uses) == "" {
				return fmt.Errorf("action step %q has no action reference", step.ID)
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
	return nil
}

func validateControlStep(step Step, backgroundIDs map[string]struct{}) error {
	if step.Background || step.Command != "" || step.Uses != "" || step.Shell != "" || step.WorkingDirectory != "" || len(step.Env) != 0 || len(step.With) != 0 || step.Condition != "" || step.TimeoutMinutes != 0 {
		return fmt.Errorf("control step %q contains incompatible execution fields", step.ID)
	}
	switch step.Kind {
	case "wait":
		if len(step.Targets) == 0 || len(step.Targets) > MaxStepTargets {
			return fmt.Errorf("wait step %q must target between 1 and %d background steps", step.ID, MaxStepTargets)
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
		if step.ContinueOnError {
			return fmt.Errorf("cancel step %q cannot continue on error", step.ID)
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
