// Package buildkite emits deterministic Buildkite pipeline YAML from validated,
// integration-neutral job descriptions.
package buildkite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const planDirectory = ".buildkite-gha/plans"
const distributionDirectory = ".buildkite-gha/distributions"
const toolDirectory = ".buildkite-gha/tools"

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Pipeline is the validated input required to emit generated compatibility jobs.
type Pipeline struct {
	CompilerStep       string
	DistributionDigest string
	MiseDigest         string
	Jobs               []Job
}

// DistributionPath returns the fixed local path for a content-addressed
// buildkite-gha executable.
func DistributionPath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid distribution digest %q", digest)
	}
	return distributionDirectory + "/" + strings.TrimPrefix(digest, "sha256:") + "/buildkite-gha", nil
}

// MisePath returns the fixed artifact path for a content-addressed mise
// executable archive.
func MisePath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid mise digest %q", digest)
	}
	return toolDirectory + "/mise/" + strings.TrimPrefix(digest, "sha256:") + "/mise.gz", nil
}

// Job describes one expanded workflow job after queue policy has been applied.
type Job struct {
	Key              string
	Label            string
	Queue            string
	PlanDigest       string
	Dependencies     []string
	ConcurrencyGroup string
	Concurrency      int
}

// PlanPath returns the fixed local path for a content-addressed job plan.
func PlanPath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid plan digest %q", digest)
	}
	return planDirectory + "/" + strings.TrimPrefix(digest, "sha256:") + ".json", nil
}

// Emit validates and emits stable YAML terminated by a newline.
func Emit(pipeline Pipeline) ([]byte, error) {
	if !validStepKey(pipeline.CompilerStep) {
		return nil, fmt.Errorf("invalid compiler step key %q", pipeline.CompilerStep)
	}
	if len(pipeline.Jobs) == 0 {
		return nil, fmt.Errorf("pipeline requires at least one generated job")
	}
	jobs, err := orderJobs(pipeline.CompilerStep, pipeline.Jobs)
	if err != nil {
		return nil, err
	}
	distributionPath, err := DistributionPath(pipeline.DistributionDigest)
	if err != nil {
		return nil, err
	}
	var misePath string
	if pipeline.MiseDigest != "" {
		misePath, err = MisePath(pipeline.MiseDigest)
		if err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	out.WriteString("steps:\n")
	for _, job := range jobs {
		planPath, err := PlanPath(job.PlanDigest)
		if err != nil {
			return nil, fmt.Errorf("job %q: %w", job.Key, err)
		}
		_, _ = fmt.Fprintf(&out, "  - label: %s\n", yamlScalar(job.Label))
		_, _ = fmt.Fprintf(&out, "    key: %s\n", yamlScalar(job.Key))
		commands := []string{
			"set -euo pipefail",
			`bootstrap_dir="$(mktemp -d "${TMPDIR:-/tmp}/buildkite-gha.XXXXXXXX")"`,
			`trap 'rm -rf -- "$bootstrap_dir"' EXIT`,
			"buildkite-agent artifact download " + shellQuote(distributionPath) + ` "$bootstrap_dir" --step ` + shellQuote(pipeline.CompilerStep),
			"buildkite-agent artifact download " + shellQuote(planPath) + ` "$bootstrap_dir" --step ` + shellQuote(pipeline.CompilerStep),
			"distribution=\"$bootstrap_dir/" + distributionPath + `"`,
			"plan=\"$bootstrap_dir/" + planPath + `"`,
			`actual_distribution_digest="$(sha256sum "$distribution" | awk '{print "sha256:" $1}')"`,
			"test \"$actual_distribution_digest\" = " + shellQuote(pipeline.DistributionDigest),
			`chmod 0500 "$distribution"`,
		}
		if misePath != "" {
			commands = append(commands,
				"buildkite-agent artifact download "+shellQuote(misePath)+` "$bootstrap_dir" --step `+shellQuote(pipeline.CompilerStep),
				"mise_archive=\"$bootstrap_dir/"+misePath+`"`,
				`mise="$bootstrap_dir/mise"`,
				`actual_mise_digest="$(sha256sum "$mise_archive" | awk '{print "sha256:" $1}')"`,
				`test "$actual_mise_digest" = `+shellQuote(pipeline.MiseDigest),
				`gzip -dc "$mise_archive" > "$mise"`,
				`chmod 0500 "$mise"`,
				`export PATH="$bootstrap_dir:$PATH"`,
			)
		}
		commands = append(commands, `"$distribution" run-job --plan "$plan"`)
		command := strings.Join(commands, "\n")
		_, _ = fmt.Fprintf(&out, "    command: %s\n", yamlScalar(command))
		out.WriteString("    agents:\n")
		_, _ = fmt.Fprintf(&out, "      queue: %s\n", yamlScalar(job.Queue))
		out.WriteString("    checkout:\n      skip: true\n")
		out.WriteString("    env:\n")
		_, _ = fmt.Fprintf(&out, "      BUILDKITE_GHA_PLAN_DIGEST: %s\n", yamlScalar(job.PlanDigest))
		_, _ = fmt.Fprintf(&out, "      BUILDKITE_GHA_PLAN_PATH: %s\n", yamlScalar(planPath))
		_, _ = fmt.Fprintf(&out, "      BUILDKITE_GHA_PLAN_PRODUCER: %s\n", yamlScalar(pipeline.CompilerStep))
		if job.Concurrency != 0 {
			_, _ = fmt.Fprintf(&out, "    concurrency: %d\n", job.Concurrency)
			_, _ = fmt.Fprintf(&out, "    concurrency_group: %s\n", yamlScalar(job.ConcurrencyGroup))
		}
		out.WriteString("    depends_on:\n")
		_, _ = fmt.Fprintf(&out, "      - step: %s\n        allow_failure: false\n", yamlScalar(pipeline.CompilerStep))
		for _, dependency := range job.Dependencies {
			_, _ = fmt.Fprintf(&out, "      - step: %s\n        allow_failure: true\n", yamlScalar(dependency))
		}
	}
	return out.Bytes(), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func orderJobs(compilerStep string, input []Job) ([]Job, error) {
	jobs := make(map[string]Job, len(input))
	digests := make(map[string]string, len(input))
	indegree := make(map[string]int, len(input))
	dependents := make(map[string][]string, len(input))
	for _, job := range input {
		if err := validateJob(compilerStep, job); err != nil {
			return nil, err
		}
		if _, exists := jobs[job.Key]; exists {
			return nil, fmt.Errorf("duplicate generated step key %q", job.Key)
		}
		if other, exists := digests[job.PlanDigest]; exists {
			return nil, fmt.Errorf("jobs %q and %q share plan digest %s", other, job.Key, job.PlanDigest)
		}
		digests[job.PlanDigest] = job.Key
		job.Dependencies = append([]string(nil), job.Dependencies...)
		sort.Strings(job.Dependencies)
		for i, dependency := range job.Dependencies {
			if i > 0 && dependency == job.Dependencies[i-1] {
				return nil, fmt.Errorf("job %q repeats dependency %q", job.Key, dependency)
			}
		}
		jobs[job.Key] = job
		indegree[job.Key] = len(job.Dependencies)
	}
	for key, job := range jobs {
		for _, dependency := range job.Dependencies {
			if _, exists := jobs[dependency]; !exists {
				return nil, fmt.Errorf("job %q has unknown dependency %q", key, dependency)
			}
			dependents[dependency] = append(dependents[dependency], key)
		}
	}
	ready := make([]string, 0, len(jobs))
	for key, degree := range indegree {
		if degree == 0 {
			ready = append(ready, key)
		}
	}
	sort.Strings(ready)
	ordered := make([]Job, 0, len(jobs))
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		ordered = append(ordered, jobs[key])
		for _, dependent := range dependents[key] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if len(ordered) != len(jobs) {
		return nil, fmt.Errorf("generated job graph contains a cycle")
	}
	return ordered, nil
}

func validateJob(compilerStep string, job Job) error {
	if !validStepKey(job.Key) || job.Key == compilerStep {
		return fmt.Errorf("invalid generated step key %q", job.Key)
	}
	if job.Label == "" {
		return fmt.Errorf("job %q requires a label", job.Key)
	}
	if !identifierPattern.MatchString(job.Queue) {
		return fmt.Errorf("job %q has invalid queue %q", job.Key, job.Queue)
	}
	if _, err := PlanPath(job.PlanDigest); err != nil {
		return fmt.Errorf("job %q: %w", job.Key, err)
	}
	if job.Concurrency < 0 {
		return fmt.Errorf("job %q has invalid concurrency %d", job.Key, job.Concurrency)
	}
	if job.Concurrency > 0 && job.ConcurrencyGroup == "" || job.Concurrency == 0 && job.ConcurrencyGroup != "" {
		return fmt.Errorf("job %q requires concurrency and concurrency group together", job.Key)
	}
	for _, dependency := range job.Dependencies {
		if !validStepKey(dependency) || dependency == compilerStep || dependency == job.Key {
			return fmt.Errorf("job %q has invalid dependency %q", job.Key, dependency)
		}
	}
	return nil
}

func validStepKey(key string) bool {
	return identifierPattern.MatchString(key) && !uuidPattern.MatchString(key)
}

func yamlScalar(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
