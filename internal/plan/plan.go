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

const Schema = "https://buildkite.com/schemas/buildkite-gha/job-plan-v1.schema.json"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var targetPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

type Compiler struct {
	Version            string `json:"version"`
	DistributionDigest string `json:"distribution_digest"`
}

type Event struct {
	Provider      string `json:"provider"`
	Name          string `json:"name"`
	PayloadDigest string `json:"payload_digest"`
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
	Command          string            `json:"command,omitempty"`
	Uses             string            `json:"uses,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	With             map[string]string `json:"with,omitempty"`
	Source           *Span             `json:"source,omitempty"`
}

// Job is one immutable, compiler-selected workflow job instance.
type Job struct {
	Schema                  string            `json:"schema"`
	Compiler                Compiler          `json:"compiler"`
	Workflow                Workflow          `json:"workflow"`
	Event                   Event             `json:"event"`
	Target                  Target            `json:"target"`
	RequiredCapabilities    []string          `json:"required_capabilities"`
	Matrix                  map[string]any    `json:"matrix,omitempty"`
	Vars                    map[string]string `json:"vars,omitempty"`
	Dependencies            []string          `json:"dependencies,omitempty"`
	Needs                   map[string]Need   `json:"needs,omitempty"`
	Env                     map[string]string `json:"env,omitempty"`
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
	if job.Schema != Schema {
		return fmt.Errorf("unsupported job plan schema %q", job.Schema)
	}
	if job.Compiler.Version == "" || !digestPattern.MatchString(job.Compiler.DistributionDigest) {
		return fmt.Errorf("job plan compiler version and distribution digest are required")
	}
	if (job.Event.Provider != "github" && job.Event.Provider != "cursor-origin") || job.Event.Name == "" || !digestPattern.MatchString(job.Event.PayloadDigest) {
		return fmt.Errorf("job plan requires a supported event binding")
	}
	if job.Workflow.Path == "" || !digestPattern.MatchString(job.Workflow.Digest) || job.Workflow.LogicalJobID == "" {
		return fmt.Errorf("job plan requires a workflow path, sha256 digest, and logical job id")
	}
	if !targetPattern.MatchString(job.Target.StepKey) || !targetPattern.MatchString(job.Target.Queue) {
		return fmt.Errorf("job plan requires a target step key and queue")
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
	if len(job.Steps) == 0 {
		return fmt.Errorf("job plan contains no steps")
	}
	needIDs := make(map[string]struct{}, len(job.Needs))
	for name := range job.Needs {
		id := strings.ToLower(name)
		if _, exists := needIDs[id]; exists {
			return fmt.Errorf("job plan contains duplicate prerequisite %q", name)
		}
		needIDs[id] = struct{}{}
	}
	ids := make(map[string]struct{}, len(job.Steps))
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
		switch step.Kind {
		case "run":
			if strings.TrimSpace(step.Command) == "" {
				return fmt.Errorf("run step %q has no command", step.ID)
			}
		case "uses":
			if strings.TrimSpace(step.Uses) == "" {
				return fmt.Errorf("action step %q has no action reference", step.ID)
			}
		default:
			return fmt.Errorf("step %q has unsupported kind %q", step.ID, step.Kind)
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
