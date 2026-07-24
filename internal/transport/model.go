// Package transport owns the Phase 0 Buildkite artifact and dynamic-pipeline
// contracts. It deliberately depends on a narrow command boundary instead of
// the Buildkite API so its behavior can be proved without a live build.
package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	ResultManifestSchema   = "buildkite-gha/result-manifest/v1"
	MaxResultOutputs       = 64
	MaxResultOutputBytes   = 1024
	MaxResultManifestBytes = 96 * 1024
	MaxResultProducers     = 256
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Digest returns the content address used by transport artifacts.
func Digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PlanArtifact is an immutable canonical plan and its compiler attribution.
type PlanArtifact struct {
	StepKey  string
	Digest   string
	Contents []byte
	Binding  []byte
}

func (a PlanArtifact) validate() error {
	if !keyPattern.MatchString(a.StepKey) {
		return fmt.Errorf("invalid plan step key %q", a.StepKey)
	}
	if !digestPattern.MatchString(a.Digest) || Digest(a.Contents) != a.Digest {
		return errors.New("plan digest does not match contents")
	}
	if len(a.Binding) == 0 {
		return errors.New("signed plan binding is required")
	}
	return nil
}

// Path is collision-resistant across plans and stable across retries.
func (a PlanArtifact) Path() string {
	return fmt.Sprintf("buildkite-gha/v1/plans/%s/%s/plan.json", a.StepKey, strings.TrimPrefix(a.Digest, "sha256:"))
}

// BindingPath is the signed Phase 0 integrity artifact beside the inert plan.
func (a PlanArtifact) BindingPath() string {
	return fmt.Sprintf("buildkite-gha/v1/plans/%s/%s/binding.jws", a.StepKey, strings.TrimPrefix(a.Digest, "sha256:"))
}

// Dependency separates the strict compiler edge from logical GHA needs.
type Dependency struct {
	StepKey string
}

// Job is the validated subset needed to emit one generated Buildkite job.
type Job struct {
	Key          string
	Label        string
	Command      string
	Queue        string
	Plan         PlanArtifact
	Dependencies []Dependency
}

// EmitTwoJobPipeline emits the deterministic Phase 0 producer/consumer spike.
// The compiler dependency is injected as strict; all supplied logical needs
// are forced to settle even when their producer failed.
func EmitTwoJobPipeline(compilerStep string, jobs []Job) ([]byte, error) {
	if !keyPattern.MatchString(compilerStep) {
		return nil, fmt.Errorf("invalid compiler step key %q", compilerStep)
	}
	if len(jobs) != 2 {
		return nil, fmt.Errorf("phase 0 transport requires exactly two generated jobs, got %d", len(jobs))
	}
	seen := map[string]bool{}
	var out bytes.Buffer
	out.WriteString("steps:\n")
	for _, job := range jobs {
		if err := validateJob(job, compilerStep, seen); err != nil {
			return nil, err
		}
		seen[job.Key] = true
		_, _ = fmt.Fprintf(&out, "  - label: %s\n", yamlString(job.Label))
		_, _ = fmt.Fprintf(&out, "    key: %s\n", yamlString(job.Key))
		_, _ = fmt.Fprintf(&out, "    command: %s\n", yamlString(job.Command))
		_, _ = fmt.Fprintf(&out, "    agents:\n      queue: %s\n", yamlString(job.Queue))
		out.WriteString("    checkout:\n      skip: true\n")
		out.WriteString("    env:\n")
		_, _ = fmt.Fprintf(&out, "      BUILDKITE_GHA_PLAN_DIGEST: %s\n", yamlString(job.Plan.Digest))
		_, _ = fmt.Fprintf(&out, "      BUILDKITE_GHA_PLAN_PRODUCER: %s\n", yamlString(compilerStep))
		out.WriteString("    depends_on:\n")
		_, _ = fmt.Fprintf(&out, "      - step: %s\n        allow_failure: false\n", yamlString(compilerStep))
		dependencies := append([]Dependency(nil), job.Dependencies...)
		sort.Slice(dependencies, func(i, j int) bool { return dependencies[i].StepKey < dependencies[j].StepKey })
		for _, dependency := range dependencies {
			_, _ = fmt.Fprintf(&out, "      - step: %s\n        allow_failure: true\n", yamlString(dependency.StepKey))
		}
	}
	return out.Bytes(), nil
}

func validateJob(job Job, compilerStep string, seen map[string]bool) error {
	if !keyPattern.MatchString(job.Key) || job.Key == compilerStep || seen[job.Key] {
		return fmt.Errorf("invalid or duplicate generated step key %q", job.Key)
	}
	if job.Label == "" || job.Command == "" || !keyPattern.MatchString(job.Queue) {
		return fmt.Errorf("job %q requires label, command, and valid queue", job.Key)
	}
	if err := job.Plan.validate(); err != nil {
		return fmt.Errorf("job %q: %w", job.Key, err)
	}
	if job.Plan.StepKey != job.Key {
		return fmt.Errorf("job %q plan is bound to %q", job.Key, job.Plan.StepKey)
	}
	dependencies := map[string]bool{}
	for _, dependency := range job.Dependencies {
		if !seen[dependency.StepKey] || dependencies[dependency.StepKey] {
			return fmt.Errorf("job %q has unknown, forward, or duplicate need %q", job.Key, dependency.StepKey)
		}
		dependencies[dependency.StepKey] = true
	}
	return nil
}

func yamlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// Output is a bounded, non-secret logical job output.
type Output struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Producer identifies the Buildkite job that owns a result manifest.
type Producer struct {
	BuildID string `json:"build_id"`
	JobID   string `json:"job_id"`
	StepKey string `json:"step_key"`
}

// Validate rejects incomplete or malformed Buildkite producer identity before
// a job runs and becomes responsible for publishing a terminal result.
func (p Producer) Validate() error {
	if !uuidPattern.MatchString(p.BuildID) || !uuidPattern.MatchString(p.JobID) || !keyPattern.MatchString(p.StepKey) {
		return errors.New("invalid result producer identity")
	}
	return nil
}

// ResultManifest is authoritative; metadata only mirrors its bounded values.
type ResultManifest struct {
	Schema     string   `json:"schema"`
	PlanDigest string   `json:"plan_digest"`
	Producer   Producer `json:"producer"`
	Result     string   `json:"result"`
	Outputs    []Output `json:"outputs"`
}

// MarshalResultManifest sorts outputs and returns stable canonical bytes.
func MarshalResultManifest(manifest ResultManifest) ([]byte, error) {
	manifest.Schema = ResultManifestSchema
	manifest.Outputs = append([]Output(nil), manifest.Outputs...)
	if manifest.Outputs == nil {
		manifest.Outputs = []Output{}
	}
	if !digestPattern.MatchString(manifest.PlanDigest) || manifest.Producer.Validate() != nil {
		return nil, errors.New("invalid result manifest identity")
	}
	switch manifest.Result {
	case "success", "failure", "cancelled", "skipped":
	default:
		return nil, fmt.Errorf("invalid logical result %q", manifest.Result)
	}
	if len(manifest.Outputs) > MaxResultOutputs {
		return nil, fmt.Errorf("result manifest has %d outputs, maximum is %d", len(manifest.Outputs), MaxResultOutputs)
	}
	sort.Slice(manifest.Outputs, func(i, j int) bool { return manifest.Outputs[i].Name < manifest.Outputs[j].Name })
	outputNames := make(map[string]struct{}, len(manifest.Outputs))
	for _, output := range manifest.Outputs {
		if !keyPattern.MatchString(output.Name) || len(output.Value) > MaxResultOutputBytes {
			return nil, fmt.Errorf("invalid or oversized output %q", output.Name)
		}
		name := strings.ToLower(output.Name)
		if _, exists := outputNames[name]; exists {
			return nil, fmt.Errorf("duplicate output %q", output.Name)
		}
		outputNames[name] = struct{}{}
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return nil, fmt.Errorf("encode result manifest: %w", err)
	}
	encoded := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(encoded) > MaxResultManifestBytes {
		return nil, fmt.Errorf("result manifest is %d bytes, maximum is %d", len(encoded), MaxResultManifestBytes)
	}
	return encoded, nil
}

// VerifyResultManifest checks both the signed-plan identity and exact artifact
// producer selected by the downloader.
func VerifyResultManifest(data []byte, expectedPlan string, expected Producer) (ResultManifest, error) {
	if len(data) > MaxResultManifestBytes {
		return ResultManifest{}, fmt.Errorf("result manifest is %d bytes, maximum is %d", len(data), MaxResultManifestBytes)
	}
	var manifest ResultManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ResultManifest{}, fmt.Errorf("decode result manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return ResultManifest{}, fmt.Errorf("decode result manifest: %w", err)
	}
	canonical, err := MarshalResultManifest(manifest)
	if err != nil {
		return ResultManifest{}, err
	}
	if !bytes.Equal(canonical, data) {
		return ResultManifest{}, errors.New("result manifest is not canonical")
	}
	if manifest.PlanDigest != expectedPlan || manifest.Producer != expected {
		return ResultManifest{}, errors.New("result manifest producer or plan binding mismatch")
	}
	return manifest, nil
}

// ResultPath is stable and requires artifact download to be constrained by the
// producer step as a separate Buildkite API argument.
func ResultPath(stepKey, planDigest string) string {
	return fmt.Sprintf("buildkite-gha/v1/results/%s/%s.json", stepKey, strings.TrimPrefix(planDigest, "sha256:"))
}

// ResultMetadata returns the visibility-only metadata mirror.
func ResultMetadata(workflow, instance string, manifest ResultManifest, manifestBytes []byte) (map[string]string, error) {
	if !keyPattern.MatchString(workflow) || !keyPattern.MatchString(instance) {
		return nil, errors.New("invalid result metadata namespace")
	}
	resultPrefix := fmt.Sprintf("buildkite-gha/v1/results/%s/%s", workflow, instance)
	outputPrefix := fmt.Sprintf("buildkite-gha/v1/outputs/%s/%s", workflow, instance)
	values := map[string]string{
		resultPrefix:                      manifest.Result,
		resultPrefix + "/manifest-digest": Digest(manifestBytes),
	}
	for _, output := range manifest.Outputs {
		values[outputPrefix+"/"+output.Name] = output.Value
	}
	return values, nil
}
