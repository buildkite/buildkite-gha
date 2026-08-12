// Package buildkite emits deterministic Buildkite pipeline YAML from validated,
// integration-neutral job descriptions.
package buildkite

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const planDirectory = ".buildkite-gha/plans"
const distributionDirectory = ".buildkite-gha/distributions"
const maxConcurrencyGroupLength = 200
const runtimeCacheName = "buildkite-gha"
const runtimeCacheRoot = "/cache/bkcache/buildkite-gha"

// ContinueOnErrorExitStatus is reserved for a workflow failure that the job's
// immutable plan explicitly allows Buildkite to soft-fail.
const ContinueOnErrorExitStatus = 78

// HostedToolCachePath is the trusted, image-baked Actions tool-cache facade
// used by explicitly selected runtime images.
const HostedToolCachePath = "/opt/hostedtoolcache"

// MinimumMiseVersion is the oldest supported mise release and the exact
// release installed when no compatible runtime executable is available.
const MinimumMiseVersion = "2026.5.12"

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var runtimeImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}$`)
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Pipeline is the validated input required to emit generated compatibility jobs.
type Pipeline struct {
	CompilerStep string
	// DistributionDigest and RuntimeImage retain the single-platform emitter
	// contract for direct callers. Mixed-platform bundles set these per Job.
	DistributionDigest string
	RuntimeImage       string
	GroupLabel         string
	ConcurrencyGate    *ConcurrencyGate
	Jobs               []Job
	Workflows          []Workflow
}

// Workflow is one independently conditioned workflow group in an aggregate
// pipeline. GroupLabel, GroupKey, and CheckName are required for aggregate
// emission.
type Workflow struct {
	GroupLabel      string
	GroupKey        string
	CheckName       string
	Condition       string
	SkipReason      string
	ConcurrencyGate *ConcurrencyGate
	Failure         *Failure
	Jobs            []Job
}

// Failure is one synthetic child step that reports a safely rendered failure.
type Failure struct {
	Label   string
	Message string
}

// ConcurrencyGate serializes an entire generated workflow while allowing the
// jobs between its opening and closing steps to run in parallel.
type ConcurrencyGate struct {
	Group string
	Queue string
}

// DistributionPath returns the fixed local path for a content-addressed
// buildkite-gha executable.
func DistributionPath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid distribution digest %q", digest)
	}
	return distributionDirectory + "/" + strings.TrimPrefix(digest, "sha256:") + "/buildkite-gha", nil
}

// MiseDataDir returns the platform-isolated managed cache path for the required
// mise version. Cache contents are an accelerator, never an authority.
func MiseDataDir(platforms ...string) string {
	platform := "linux/amd64"
	if len(platforms) != 0 {
		platform = platforms[0]
	}
	return runtimeCacheRoot + "/mise/" + platformCacheKey(platform) + "/" + MinimumMiseVersion
}

// Job describes one expanded workflow job after queue policy has been applied.
type Job struct {
	Key                string
	Label              string
	Queue              string
	Platform           string
	DistributionDigest string
	RuntimeImage       string
	PlanDigest         string
	Dependencies       []string
	RequiresMise       bool
	SoftFail           bool
	ConcurrencyGroup   string
	Concurrency        int
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
	aggregate := len(pipeline.Workflows) != 0
	workflows := pipeline.Workflows
	if aggregate {
		if len(pipeline.Jobs) != 0 || pipeline.GroupLabel != "" || pipeline.ConcurrencyGate != nil {
			return nil, fmt.Errorf("aggregate pipeline cannot mix legacy workflow fields")
		}
	} else {
		workflows = []Workflow{{
			GroupLabel:      pipeline.GroupLabel,
			ConcurrencyGate: pipeline.ConcurrencyGate,
			Jobs:            pipeline.Jobs,
		}}
	}

	prepared := make([]preparedWorkflow, len(workflows))
	usedKeys := map[string]string{pipeline.CompilerStep: "compiler step"}
	usedDigests := make(map[string]string)
	for i, workflow := range workflows {
		if len(workflow.Jobs) == 0 && workflow.Failure == nil && (!aggregate || workflow.SkipReason == "") {
			return nil, fmt.Errorf("workflow %d requires at least one generated job", i+1)
		}
		if aggregate {
			if workflow.GroupLabel == "" {
				return nil, fmt.Errorf("workflow %d requires a group label", i+1)
			}
			if !validStepKey(workflow.GroupKey) {
				return nil, fmt.Errorf("workflow %d has invalid group key %q", i+1, workflow.GroupKey)
			}
			if workflow.CheckName == "" {
				return nil, fmt.Errorf("workflow %q requires a GitHub Check name", workflow.GroupKey)
			}
			if workflow.Condition == "" && workflow.SkipReason == "" {
				return nil, fmt.Errorf("workflow %q requires a trigger condition or skip reason", workflow.GroupKey)
			}
			if workflow.Condition != "" && workflow.SkipReason != "" {
				return nil, fmt.Errorf("workflow %q cannot have both a trigger condition and skip reason", workflow.GroupKey)
			}
			if workflow.SkipReason != "" && workflow.Failure != nil {
				return nil, fmt.Errorf("workflow %q cannot have both failures and skip reason", workflow.GroupKey)
			}
			if utf8.RuneCountInString(workflow.SkipReason) > maxSkipReasonLength {
				return nil, fmt.Errorf("workflow %q skip reason exceeds %d characters", workflow.GroupKey, maxSkipReasonLength)
			}
			if owner, exists := usedKeys[workflow.GroupKey]; exists {
				return nil, fmt.Errorf("workflow group key %q collides with %s", workflow.GroupKey, owner)
			}
			usedKeys[workflow.GroupKey] = "workflow group"
		}
		if workflow.Failure != nil {
			if workflow.Failure.Label == "" || workflow.Failure.Message == "" {
				return nil, fmt.Errorf("workflow %d failure requires a label and message", i+1)
			}
		}
		jobs, err := orderJobs(pipeline.CompilerStep, workflow.Jobs)
		if err != nil {
			return nil, err
		}
		if err := validateConcurrencyGate(workflow.ConcurrencyGate); err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if owner, exists := usedKeys[job.Key]; exists {
				return nil, fmt.Errorf("generated step key %q collides with %s", job.Key, owner)
			}
			usedKeys[job.Key] = "generated job"
			if owner, exists := usedDigests[job.PlanDigest]; exists {
				return nil, fmt.Errorf("jobs %q and %q share plan digest %s", owner, job.Key, job.PlanDigest)
			}
			usedDigests[job.PlanDigest] = job.Key
		}
		prepared[i] = preparedWorkflow{Workflow: workflow, Jobs: jobs, Grouped: aggregate || workflow.GroupLabel != "", Aggregate: aggregate}
		if workflow.ConcurrencyGate != nil {
			gateNamespace := pipeline.CompilerStep
			if aggregate {
				gateNamespace += "\x00" + workflow.GroupKey
			}
			prepared[i].GateOpenKey, prepared[i].GateCloseKey = concurrencyGateKeys(gateNamespace, workflow.ConcurrencyGate.Group, jobs)
			for _, gateKey := range []string{prepared[i].GateOpenKey, prepared[i].GateCloseKey} {
				if owner, exists := usedKeys[gateKey]; exists {
					return nil, fmt.Errorf("workflow concurrency key %q collides with %s", gateKey, owner)
				}
				usedKeys[gateKey] = "workflow concurrency gate"
			}
		}
	}
	var out bytes.Buffer
	out.WriteString("steps:\n")
	for _, workflow := range prepared {
		if err := emitWorkflow(&out, pipeline, workflow); err != nil {
			return nil, err
		}
	}
	return out.Bytes(), nil
}

type preparedWorkflow struct {
	Workflow
	Jobs                      []Job
	Grouped                   bool
	Aggregate                 bool
	GateOpenKey, GateCloseKey string
}

func emitWorkflow(out *bytes.Buffer, pipeline Pipeline, workflow preparedWorkflow) error {
	stepIndent := "  "
	if workflow.Grouped {
		groupLabel := workflow.GroupLabel
		if workflow.Aggregate {
			groupLabel = ":github: " + groupLabel
		}
		_, _ = fmt.Fprintf(out, "  - group: %s\n", yamlScalar(groupLabel))
		if workflow.GroupKey != "" {
			_, _ = fmt.Fprintf(out, "    key: %s\n", yamlScalar(workflow.GroupKey))
		}
		if workflow.Condition != "" {
			_, _ = fmt.Fprintf(out, "    if: %s\n", yamlScalar(workflow.Condition))
		}
		if workflow.SkipReason != "" {
			_, _ = fmt.Fprintf(out, "    skip: %s\n", yamlScalar(workflow.SkipReason))
		}
		if workflow.CheckName != "" {
			out.WriteString("    notify:\n")
			out.WriteString("      - github_check:\n")
			_, _ = fmt.Fprintf(out, "          name: %s\n", yamlScalar(workflow.CheckName))
		}
		if workflow.Aggregate {
			_, _ = fmt.Fprintf(out, "    depends_on: %s\n", yamlScalar(pipeline.CompilerStep))
		}
		out.WriteString("    steps:\n")
		stepIndent = "      "
	}
	attributeIndent := stepIndent + "  "
	if workflow.SkipReason != "" && len(workflow.Jobs) == 0 {
		_, _ = fmt.Fprintf(out, "%s- label: %s\n", stepIndent, yamlScalar("Ignored workflow"))
		_, _ = fmt.Fprintf(out, "%scommand: %s\n", attributeIndent, yamlScalar(":"))
		_, _ = fmt.Fprintf(out, "%scheckout:\n%s  skip: true\n", attributeIndent, attributeIndent)
		return nil
	}
	if failure := workflow.Failure; failure != nil {
		_, _ = fmt.Fprintf(out, "%s- label: %s\n", stepIndent, yamlScalar(failure.Label))
		command := `printf '%s\n' ` + shellQuote(failure.Message) + ` && exit 1`
		_, _ = fmt.Fprintf(out, "%scommand: %s\n", attributeIndent, yamlScalar(command))
		_, _ = fmt.Fprintf(out, "%scheckout:\n%s  skip: true\n", attributeIndent, attributeIndent)
	}
	if workflow.ConcurrencyGate != nil {
		dependencies := []dependency{{Step: pipeline.CompilerStep}}
		if workflow.Aggregate {
			dependencies = nil
		}
		emitConcurrencyGateStep(out, stepIndent, attributeIndent, ":github: Start workflow concurrency", workflow.GateOpenKey, workflow.ConcurrencyGate, dependencies)
	}
	for _, job := range workflow.Jobs {
		platform := job.Platform
		if platform == "" {
			platform = "linux/amd64"
		}
		distributionDigest := job.DistributionDigest
		if distributionDigest == "" {
			distributionDigest = pipeline.DistributionDigest
		}
		runtimeImage := job.RuntimeImage
		if runtimeImage == "" {
			runtimeImage = pipeline.RuntimeImage
		}
		distributionPath, err := DistributionPath(distributionDigest)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Key, err)
		}
		if platform != "linux/amd64" && platform != "darwin/arm64" {
			return fmt.Errorf("job %q has unsupported runtime platform %q", job.Key, platform)
		}
		if runtimeImage != "" && !runtimeImagePattern.MatchString(runtimeImage) {
			return fmt.Errorf("job %q runtime image %q must be an immutable registry sha256 reference", job.Key, runtimeImage)
		}
		if platform == "darwin/arm64" && runtimeImage != "" {
			return fmt.Errorf("job %q cannot select a container runtime image on darwin/arm64", job.Key)
		}
		_, _ = fmt.Fprintf(out, "%s- label: %s\n", stepIndent, yamlScalar(job.Label))
		_, _ = fmt.Fprintf(out, "%skey: %s\n", attributeIndent, yamlScalar(job.Key))
		if runtimeImage != "" {
			_, _ = fmt.Fprintf(out, "%simage: %s\n", attributeIndent, yamlScalar(runtimeImage))
		}
		commands := []string{
			"set -euo pipefail",
			`bootstrap_dir="$(mktemp -d "${TMPDIR:-/tmp}/buildkite-gha.XXXXXXXX")"`,
			`trap 'rm -rf -- "$bootstrap_dir"' EXIT`,
			"buildkite-agent artifact download " + shellQuote(distributionPath) + ` "$bootstrap_dir" --step ` + shellQuote(pipeline.CompilerStep),
			"distribution=\"$bootstrap_dir/" + distributionPath + `"`,
			`if command -v sha256sum >/dev/null 2>&1; then actual_distribution_digest="$(sha256sum "$distribution" | awk '{print "sha256:" $1}')"; elif command -v shasum >/dev/null 2>&1; then actual_distribution_digest="$(shasum -a 256 "$distribution" | awk '{print "sha256:" $1}')"; else echo 'buildkite-gha: no SHA-256 tool available' >&2; exit 1; fi`,
			"test \"$actual_distribution_digest\" = " + shellQuote(distributionDigest),
			`chmod 0500 "$distribution"`,
		}
		runJob := `"$distribution" run-job --plan-digest ` + shellQuote(job.PlanDigest) + " --plan-producer " + shellQuote(pipeline.CompilerStep)
		if runtimeImage != "" {
			runJob += " --hosted-tool-cache"
		}
		commands = append(commands, runJob)
		command := strings.Join(commands, "\n")
		_, _ = fmt.Fprintf(out, "%scommand: %s\n", attributeIndent, yamlScalar(command))
		if job.Queue != "" {
			_, _ = fmt.Fprintf(out, "%sagents:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s  queue: %s\n", attributeIndent, yamlScalar(job.Queue))
		}
		_, _ = fmt.Fprintf(out, "%scheckout:\n%s  skip: true\n", attributeIndent, attributeIndent)
		if job.RequiresMise {
			_, _ = fmt.Fprintf(out, "%scache:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s  paths:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s    - %s\n", attributeIndent, yamlScalar(platformMiseCachePath(platform)))
			_, _ = fmt.Fprintf(out, "%s  name: %s\n", attributeIndent, yamlScalar(runtimeCacheName+"-"+platformCacheKey(platform)))
		}
		if job.SoftFail {
			_, _ = fmt.Fprintf(out, "%ssoft_fail:\n%s  - exit_status: %d\n", attributeIndent, attributeIndent, ContinueOnErrorExitStatus)
		}
		if job.RequiresMise {
			_, _ = fmt.Fprintf(out, "%senv:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s  BUILDKITE_GHA_MISE_DATA_DIR: %s\n", attributeIndent, yamlScalar(MiseDataDir(platform)))
		}
		if job.Concurrency != 0 {
			_, _ = fmt.Fprintf(out, "%sconcurrency: %d\n", attributeIndent, job.Concurrency)
			_, _ = fmt.Fprintf(out, "%sconcurrency_group: %s\n", attributeIndent, yamlScalar(job.ConcurrencyGroup))
		}
		if !workflow.Aggregate {
			_, _ = fmt.Fprintf(out, "%sdepends_on:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: false\n", attributeIndent, yamlScalar(pipeline.CompilerStep), attributeIndent)
		} else if workflow.GateOpenKey != "" || len(job.Dependencies) != 0 {
			_, _ = fmt.Fprintf(out, "%sdepends_on:\n", attributeIndent)
		}
		if workflow.GateOpenKey != "" {
			_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: false\n", attributeIndent, yamlScalar(workflow.GateOpenKey), attributeIndent)
		}
		for _, dependency := range job.Dependencies {
			_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: true\n", attributeIndent, yamlScalar(dependency), attributeIndent)
		}
	}
	if workflow.ConcurrencyGate != nil {
		dependencies := make([]dependency, 0, len(workflow.Jobs)+1)
		if !workflow.Aggregate {
			dependencies = append(dependencies, dependency{Step: pipeline.CompilerStep})
		}
		for _, job := range workflow.Jobs {
			dependencies = append(dependencies, dependency{Step: job.Key, AllowFailure: true})
		}
		emitConcurrencyGateStep(out, stepIndent, attributeIndent, ":github: Finish workflow concurrency", workflow.GateCloseKey, workflow.ConcurrencyGate, dependencies)
	}
	return nil
}

func platformCacheKey(platform string) string {
	return strings.ReplaceAll(platform, "/", "-")
}

func platformMiseCachePath(platform string) string {
	return runtimeCacheRoot + "/mise/" + platformCacheKey(platform)
}

type dependency struct {
	Step         string
	AllowFailure bool
}

func emitConcurrencyGateStep(out *bytes.Buffer, stepIndent, attributeIndent, label, key string, gate *ConcurrencyGate, dependencies []dependency) {
	_, _ = fmt.Fprintf(out, "%s- label: %s\n", stepIndent, yamlScalar(label))
	_, _ = fmt.Fprintf(out, "%skey: %s\n", attributeIndent, yamlScalar(key))
	_, _ = fmt.Fprintf(out, "%scommand: %s\n", attributeIndent, yamlScalar("true"))
	if gate.Queue != "" {
		_, _ = fmt.Fprintf(out, "%sagents:\n", attributeIndent)
		_, _ = fmt.Fprintf(out, "%s  queue: %s\n", attributeIndent, yamlScalar(gate.Queue))
	}
	_, _ = fmt.Fprintf(out, "%scheckout:\n%s  skip: true\n", attributeIndent, attributeIndent)
	_, _ = fmt.Fprintf(out, "%sconcurrency: 1\n", attributeIndent)
	_, _ = fmt.Fprintf(out, "%sconcurrency_group: %s\n", attributeIndent, yamlScalar(gate.Group))
	if len(dependencies) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "%sdepends_on:\n", attributeIndent)
	for _, dependency := range dependencies {
		_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: %t\n", attributeIndent, yamlScalar(dependency.Step), attributeIndent, dependency.AllowFailure)
	}
}

func validateConcurrencyGate(gate *ConcurrencyGate) error {
	if gate == nil {
		return nil
	}
	if gate.Group == "" || len(gate.Group) > maxConcurrencyGroupLength {
		return fmt.Errorf("invalid workflow concurrency group")
	}
	if gate.Queue != "" && !identifierPattern.MatchString(gate.Queue) {
		return fmt.Errorf("workflow concurrency gate has invalid queue %q", gate.Queue)
	}
	return nil
}

func concurrencyGateKeys(compilerStep, group string, jobs []Job) (string, string) {
	digest := sha256Sum(compilerStep + "\x00" + group)
	used := map[string]struct{}{compilerStep: {}}
	for _, job := range jobs {
		used[job.Key] = struct{}{}
	}
	open := uniqueStepKey("gha-concurrency-start-"+digest, used)
	used[open] = struct{}{}
	close := uniqueStepKey("gha-concurrency-finish-"+digest, used)
	return open, close
}

func sha256Sum(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:6])
}

func uniqueStepKey(base string, used map[string]struct{}) string {
	if _, exists := used[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
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
	if job.Queue != "" && !identifierPattern.MatchString(job.Queue) {
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
	if len(job.ConcurrencyGroup) > maxConcurrencyGroupLength {
		return fmt.Errorf("job %q concurrency group exceeds %d characters", job.Key, maxConcurrencyGroupLength)
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
