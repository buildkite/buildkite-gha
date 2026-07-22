// Package compiler expands an owned workflow into deterministic Phase 0 JSON IR.
package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/buildkite/buildkite-gha/internal/workflow"
)

const (
	schema             = "buildkite-gha/compiler-ir/v0"
	maxMatrixInstances = 256
)

// Event is the explicit provider event snapshot supplied to compilation.
type Event struct {
	Provider   string         `json:"provider"`
	Event      string         `json:"event"`
	Repository Repository     `json:"repository"`
	Ref        string         `json:"ref"`
	SHA        string         `json:"sha"`
	Actor      string         `json:"actor"`
	Payload    map[string]any `json:"payload"`
}

// Repository identifies the source repository in the event snapshot.
type Repository struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

// IR is the deterministic, actionlint-independent Phase 0 compiler output.
type IR struct {
	Schema    string            `json:"schema"`
	Workflow  WorkflowSource    `json:"workflow"`
	Event     Event             `json:"event"`
	Execution ExecutionBoundary `json:"execution"`
	Jobs      []JobInstance     `json:"jobs"`
}

// ExecutionBoundary makes the compile-only Phase 0 boundary explicit.
type ExecutionBoundary struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// WorkflowSource binds the IR to its input workflow bytes.
type WorkflowSource struct {
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest"`
}

// JobInstance is one statically expanded job in the owned IR.
type JobInstance struct {
	Key          string          `json:"key"`
	LogicalJobID string          `json:"logical_job_id"`
	Label        string          `json:"label"`
	Needs        []string        `json:"needs,omitempty"`
	RunsOn       []string        `json:"runs_on"`
	Matrix       map[string]any  `json:"matrix,omitempty"`
	FailFast     *bool           `json:"fail_fast,omitempty"`
	MaxParallel  *int            `json:"max_parallel,omitempty"`
	Steps        []workflow.Step `json:"steps"`
	Source       workflow.Span   `json:"source"`
}

// Report summarizes successful workflow validation.
type Report struct {
	LogicalJobs int
	Instances   int
}

// Validate parses and validates the supported static graph without requiring an
// event snapshot.
func Validate(path string, source []byte) (Report, error) {
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return Report{}, err
	}
	instances, err := expand(path, parsed)
	if err != nil {
		return Report{}, err
	}
	return Report{LogicalJobs: len(parsed.Jobs), Instances: len(instances)}, nil
}

// ValidateEvent validates both the supported static graph and its event input.
func ValidateEvent(path string, source, eventSource []byte) (Report, error) {
	if _, err := parseEvent(eventSource); err != nil {
		return Report{}, err
	}
	return Validate(path, source)
}

// Compile parses a workflow and event, expands its static graph, and returns
// stable JSON bytes terminated by a newline.
func Compile(path string, source, eventSource []byte) ([]byte, error) {
	parsed, err := workflow.Parse(path, source)
	if err != nil {
		return nil, err
	}
	event, err := parseEvent(eventSource)
	if err != nil {
		return nil, err
	}
	jobs, err := expand(path, parsed)
	if err != nil {
		return nil, err
	}

	digest := sha256.Sum256(source)
	ir := IR{
		Schema: schema,
		Workflow: WorkflowSource{
			Path:   path,
			Name:   parsed.Name,
			Digest: "sha256:" + hex.EncodeToString(digest[:]),
		},
		Event: event,
		Execution: ExecutionBoundary{
			Supported: false,
			Reason:    "Phase 0 emits static compiler IR; run-job execution is not implemented",
		},
		Jobs: jobs,
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(ir); err != nil {
		return nil, fmt.Errorf("encode compiler IR: %w", err)
	}
	return out.Bytes(), nil
}

func parseEvent(source []byte) (Event, error) {
	if len(bytes.TrimSpace(source)) == 0 {
		return Event{}, fmt.Errorf("event snapshot is required")
	}
	var event Event
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("parse event snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Event{}, fmt.Errorf("parse event snapshot: multiple JSON values")
		}
		return Event{}, fmt.Errorf("parse event snapshot: %w", err)
	}
	if event.Provider == "" || event.Event == "" || event.Repository.Owner == "" || event.Repository.Name == "" || event.SHA == "" {
		return Event{}, fmt.Errorf("event snapshot requires provider, event, repository owner/name, and sha")
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	return event, nil
}

func expand(path string, parsed *workflow.Workflow) ([]JobInstance, error) {
	jobs := make(map[string]workflow.Job, len(parsed.Jobs))
	for _, job := range parsed.Jobs {
		jobs[job.ID] = job
		if err := supported(path, job); err != nil {
			return nil, err
		}
	}
	order, err := topologicalOrder(path, jobs)
	if err != nil {
		return nil, err
	}

	byLogicalID := make(map[string][]JobInstance, len(jobs))
	for _, id := range order {
		job := jobs[id]
		matrices, err := expandMatrix(path, job)
		if err != nil {
			return nil, err
		}
		for _, matrix := range matrices {
			instance := JobInstance{
				Key:          instanceKey(job.ID, matrix),
				LogicalJobID: job.ID,
				Label:        instanceLabel(job, matrix),
				RunsOn:       append([]string(nil), job.RunsOn...),
				Matrix:       matrix,
				FailFast:     job.FailFast,
				MaxParallel:  job.MaxParallel,
				Steps:        append([]workflow.Step(nil), job.Steps...),
				Source:       job.Span,
			}
			for _, need := range job.Needs {
				for _, prerequisite := range byLogicalID[need] {
					instance.Needs = append(instance.Needs, prerequisite.Key)
				}
			}
			sort.Strings(instance.Needs)
			byLogicalID[id] = append(byLogicalID[id], instance)
		}
	}

	var instances []JobInstance
	for _, id := range order {
		instances = append(instances, byLogicalID[id]...)
	}
	return instances, nil
}

func supported(path string, job workflow.Job) error {
	if job.Reusable {
		return jobError(path, job, "reusable-workflow jobs are unsupported in the Phase 0 compiler")
	}
	if job.RunsOnExpr != nil {
		return locatedJobError(path, job, job.RunsOnExpr.Span.Start.Line, job.RunsOnExpr.Span.Start.Column, "runtime-dependent runs-on expressions are unsupported")
	}
	if len(job.RunsOn) == 0 {
		return jobError(path, job, "runs-on must resolve statically")
	}
	if job.Matrix == nil {
		return nil
	}
	if job.Matrix.Expression != nil {
		return locatedJobError(path, job, job.Matrix.Expression.Span.Start.Line, job.Matrix.Expression.Span.Start.Column, "runtime-dependent matrix expressions are unsupported")
	}
	if job.Matrix.HasInclude || job.Matrix.HasExclude {
		return locatedJobError(path, job, job.Matrix.Span.Start.Line, job.Matrix.Span.Start.Column, "matrix include/exclude are unsupported in the Phase 0 compiler")
	}
	for _, row := range job.Matrix.Rows {
		if row.Expression != nil {
			return locatedJobError(path, job, row.Expression.Span.Start.Line, row.Expression.Span.Start.Column, fmt.Sprintf("runtime-dependent matrix expression for %q is unsupported", row.Name))
		}
	}
	return nil
}

func expandMatrix(path string, job workflow.Job) ([]map[string]any, error) {
	if job.Matrix == nil {
		return []map[string]any{nil}, nil
	}
	matrices := []map[string]any{{}}
	for _, row := range job.Matrix.Rows {
		if len(row.Values) == 0 {
			return nil, jobError(path, job, fmt.Sprintf("matrix dimension %q has no values", row.Name))
		}
		var next []map[string]any
		for _, current := range matrices {
			for _, value := range row.Values {
				combination := make(map[string]any, len(current)+1)
				for key, existing := range current {
					combination[key] = existing
				}
				combination[row.Name] = value.Data
				next = append(next, combination)
				if len(next) > maxMatrixInstances {
					return nil, jobError(path, job, fmt.Sprintf("matrix expands beyond %d instances", maxMatrixInstances))
				}
			}
		}
		matrices = next
	}
	return matrices, nil
}

func topologicalOrder(path string, jobs map[string]workflow.Job) ([]string, error) {
	indegree := make(map[string]int, len(jobs))
	dependents := make(map[string][]string, len(jobs))
	for id, job := range jobs {
		indegree[id] = len(job.Needs)
		for _, need := range job.Needs {
			if _, ok := jobs[need]; !ok {
				return nil, jobError(path, job, fmt.Sprintf("needs unknown job %q", need))
			}
			dependents[need] = append(dependents[need], id)
		}
	}
	var ready []string
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) != 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(jobs) {
		var cyclic []string
		for id, degree := range indegree {
			if degree != 0 {
				cyclic = append(cyclic, id)
			}
		}
		sort.Strings(cyclic)
		return nil, jobError(path, jobs[cyclic[0]], "workflow job graph contains a cycle")
	}
	return order, nil
}

func instanceKey(jobID string, matrix map[string]any) string {
	prefix := "gha-" + sanitize(jobID)
	if len(matrix) == 0 {
		return prefix
	}
	canonical, _ := json.Marshal(matrix)
	digest := sha256.Sum256(canonical)
	return prefix + "-" + hex.EncodeToString(digest[:6])
}

func instanceLabel(job workflow.Job, matrix map[string]any) string {
	label := job.Name
	if label == "" {
		label = job.ID
	}
	if len(matrix) == 0 {
		return label
	}
	keys := make([]string, 0, len(matrix))
	for key := range matrix {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, fmt.Sprintf("%s=%v", key, matrix[key]))
	}
	return label + " (" + strings.Join(values, ", ") + ")"
}

func sanitize(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			out.WriteRune(r)
		} else if out.Len() > 0 {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func jobError(path string, job workflow.Job, message string) error {
	return locatedJobError(path, job, job.Span.Start.Line, job.Span.Start.Column, message)
}

func locatedJobError(path string, job workflow.Job, line, column int, message string) error {
	return fmt.Errorf("%s:%d:%d: job %q: %s", path, line, column, job.ID, message)
}
