// Package buildkite emits deterministic Buildkite pipeline YAML from validated,
// integration-neutral job descriptions.
package buildkite

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const planDirectory = ".buildkite-gha/plans"
const distributionDirectory = ".buildkite-gha/distributions"
const maxConcurrencyGroupLength = 200
const runtimeCacheName = "buildkite-gha"
const runtimeCacheRoot = "/cache/bkcache/buildkite-gha"
const darwinRuntimeCacheRoot = "/tmp/bkcache/buildkite-gha"

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
var cacheNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,98}[A-Za-z0-9])?$`)
var cacheNameVariablePattern = regexp.MustCompile(`\$\{BUILDKITE_[A-Z0-9_]+\}`)
var cacheSizePattern = regexp.MustCompile(`^[0-9]+g$`)

// CacheVolume is one Buildkite Hosted cache volume attached to a generated job.
type CacheVolume struct {
	Paths []string
	Name  string
	Size  string
}

// ValidateCacheVolume validates Buildkite's map-form cache volume contract.
func ValidateCacheVolume(cache CacheVolume) error {
	if len(cache.Paths) == 0 {
		return fmt.Errorf("cache paths must be a non-empty array of non-empty strings")
	}
	seen := make(map[string]struct{}, len(cache.Paths))
	for i, path := range cache.Paths {
		if strings.TrimSpace(path) == "" || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return fmt.Errorf("cache paths entry %d must be a non-empty string", i)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("cache path %q must be absolute", path)
		}
		if _, duplicate := seen[path]; duplicate {
			return fmt.Errorf("cache path %q may only be configured once", path)
		}
		seen[path] = struct{}{}
	}
	if cache.Name != "" {
		literalShape := cacheNameVariablePattern.ReplaceAllString(cache.Name, "a")
		if len(cache.Name) > 100 || strings.Contains(literalShape, "$") || !cacheNamePattern.MatchString(literalShape) {
			return fmt.Errorf("cache name must be at most 100 characters, use only letters, numbers, hyphens, and ${BUILDKITE_*} variables, and start and end with a letter, number, or variable")
		}
	}
	if cache.Size != "" {
		if !cacheSizePattern.MatchString(cache.Size) {
			return fmt.Errorf("cache size must be at least 20 gigabytes in Ng format")
		}
		gigabytes := strings.TrimLeft(strings.TrimSuffix(cache.Size, "g"), "0")
		if len(gigabytes) < 2 || (len(gigabytes) == 2 && gigabytes < "20") {
			return fmt.Errorf("cache size must be at least 20 gigabytes in Ng format")
		}
	}
	return nil
}

// Pipeline is the validated input required to emit generated compatibility jobs.
type Pipeline struct {
	CompilerStep string
	// DistributionDigest and RuntimeImage retain the single-platform emitter
	// contract for direct callers. Mixed-platform bundles set these per Job.
	DistributionDigest string
	RuntimeImage       string
	GroupLabel         string
	EventProvider      string
	ConcurrencyGate    *ConcurrencyGate
	DisableRunnerUser  bool
	Jobs               []Job
	Workflows          []Workflow
}

// Workflow is one independently conditioned workflow group in an aggregate
// pipeline. GroupLabel, GroupKey, and Event are required for aggregate emission.
type Workflow struct {
	GroupLabel      string
	CheckName       string
	GroupKey        string
	Event           string
	Condition       string
	SkipReason      string
	ConcurrencyGate *ConcurrencyGate
	Failure         *Failure
	Jobs            []Job
}

// Failure replaces a failed aggregate workflow with one synthetic command step.
type Failure struct {
	AnnotationPath string
	MessagePath    string
	Summary        string
}

// ConcurrencyGate serializes one workflow scope while allowing its jobs to run
// in parallel. ID identifies called-workflow scopes; the root scope leaves it empty.
type ConcurrencyGate struct {
	ID    string
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
	return platformMiseCachePath(platform) + "/" + MinimumMiseVersion
}

// Job describes one expanded workflow job after queue policy has been applied.
type Job struct {
	Key                string
	Label              string
	CheckLabel         string
	Queue              string
	Platform           string
	DistributionDigest string
	RuntimeImage       string
	PlanDigest         string
	Dependencies       []string
	RequiresMise       bool
	Cache              *CacheVolume
	SoftFail           bool
	ConcurrencyGroup   string
	Concurrency        int
	ConcurrencyGates   []ConcurrencyGate
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
		if pipeline.EventProvider != "github" && pipeline.EventProvider != "cursor-origin" {
			return nil, fmt.Errorf("aggregate pipeline has unsupported event provider %q", pipeline.EventProvider)
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
			if workflow.Event == "" {
				return nil, fmt.Errorf("workflow %q requires an event name", workflow.GroupKey)
			}
			if workflow.Failure == nil && workflow.Condition == "" && workflow.SkipReason == "" {
				return nil, fmt.Errorf("workflow %q requires a trigger condition or skip reason", workflow.GroupKey)
			}
			if workflow.Failure == nil && workflow.Condition != "" && workflow.SkipReason != "" {
				return nil, fmt.Errorf("workflow %q cannot have both a trigger condition and skip reason", workflow.GroupKey)
			}
			if workflow.Failure == nil && utf8.RuneCountInString(workflow.SkipReason) > maxSkipReasonLength {
				return nil, fmt.Errorf("workflow %q skip reason exceeds %d characters", workflow.GroupKey, maxSkipReasonLength)
			}
			if owner, exists := usedKeys[workflow.GroupKey]; exists {
				return nil, fmt.Errorf("workflow group key %q collides with %s", workflow.GroupKey, owner)
			}
			usedKeys[workflow.GroupKey] = "workflow group"
		}
		if workflow.Failure != nil {
			if !validArtifactPath(workflow.Failure.AnnotationPath) {
				return nil, fmt.Errorf("workflow %d failure requires a valid annotation artifact path", i+1)
			}
			if !validArtifactPath(workflow.Failure.MessagePath) {
				return nil, fmt.Errorf("workflow %d failure requires a valid message artifact path", i+1)
			}
			if workflow.Failure.Summary == "" {
				return nil, fmt.Errorf("workflow %d failure requires a provider check summary", i+1)
			}
			if len(workflow.Jobs) != 0 || workflow.ConcurrencyGate != nil {
				return nil, fmt.Errorf("workflow %d failure cannot include jobs or a concurrency gate", i+1)
			}
		}
		jobs, err := orderJobs(pipeline.CompilerStep, workflow.Jobs)
		if err != nil {
			return nil, err
		}
		checkLabels := make(map[string]string, len(jobs))
		if err := validateConcurrencyGate(workflow.ConcurrencyGate); err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if owner, exists := usedKeys[job.Key]; exists {
				return nil, fmt.Errorf("generated step key %q collides with %s", job.Key, owner)
			}
			usedKeys[job.Key] = "generated job"
			if aggregate {
				checkLabel := job.CheckLabel
				if checkLabel == "" {
					checkLabel = job.Label
				}
				if owner, exists := checkLabels[checkLabel]; exists {
					return nil, fmt.Errorf("workflow %q jobs %q and %q share provider check label %q", workflow.GroupKey, owner, job.Key, checkLabel)
				}
				checkLabels[checkLabel] = job.Key
			}
			if owner, exists := usedDigests[job.PlanDigest]; exists {
				return nil, fmt.Errorf("jobs %q and %q share plan digest %s", owner, job.Key, job.PlanDigest)
			}
			usedDigests[job.PlanDigest] = job.Key
		}
		if workflow.ConcurrencyGate != nil {
			for _, job := range jobs {
				if job.Concurrency > 0 && job.ConcurrencyGroup == workflow.ConcurrencyGate.Group {
					return nil, fmt.Errorf("workflow concurrency gate shares group with member job %q", job.Key)
				}
			}
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
		gates, err := prepareReusableConcurrencyGates(jobs)
		if err != nil {
			return nil, err
		}
		if workflow.ConcurrencyGate != nil {
			for _, gate := range gates {
				if gate.Group == workflow.ConcurrencyGate.Group {
					return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with enclosing workflow gate", gate.ID)
				}
			}
		}
		for gateIndex := range gates {
			gate := &gates[gateIndex]
			gateNamespace := pipeline.CompilerStep + "\x00" + gate.ID
			if aggregate {
				gateNamespace += "\x00" + workflow.GroupKey
			}
			gate.OpenKey, gate.CloseKey = concurrencyGateKeys(gateNamespace, gate.Group, jobs)
			for _, gateKey := range []string{gate.OpenKey, gate.CloseKey} {
				if owner, exists := usedKeys[gateKey]; exists {
					return nil, fmt.Errorf("reusable-workflow concurrency key %q collides with %s", gateKey, owner)
				}
				usedKeys[gateKey] = "reusable-workflow concurrency gate"
			}
		}
		prepared[i].ReusableConcurrencyGates = gates
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
	ReusableConcurrencyGates  []preparedConcurrencyGate
}

type preparedConcurrencyGate struct {
	ConcurrencyGate
	ParentID, OpenKey, CloseKey string
	Members                     []string
}

func emitWorkflow(out *bytes.Buffer, pipeline Pipeline, workflow preparedWorkflow) error {
	if failure := workflow.Failure; failure != nil {
		_, _ = fmt.Fprintf(out, "  - label: %s\n", yamlScalar(":github: workflow · "+workflow.GroupLabel))
		_, _ = fmt.Fprintf(out, "    key: %s\n", yamlScalar(workflow.GroupKey))
		if workflow.Condition != "" {
			_, _ = fmt.Fprintf(out, "    if: %s\n", yamlScalar(workflow.Condition))
		}
		out.WriteString("    plugins:\n")
		out.WriteString("      - artifacts#v1.9.4:\n")
		_, _ = fmt.Fprintf(out, "          step: %s\n", yamlScalar(pipeline.CompilerStep))
		out.WriteString("          download:\n")
		_, _ = fmt.Fprintf(out, "            - from: %s\n", yamlScalar(failure.MessagePath))
		out.WriteString("              to: .buildkite-gha-failure-message.txt\n")
		_, _ = fmt.Fprintf(out, "            - from: %s\n", yamlScalar(failure.AnnotationPath))
		out.WriteString("              to: .buildkite-gha-failure-annotation.html\n")
		commands := []string{
			"cat .buildkite-gha-failure-message.txt",
			"buildkite-agent annotate --scope=job --style=error < .buildkite-gha-failure-annotation.html",
			"exit 1",
		}
		command := strings.Join(commands, "\n")
		_, _ = fmt.Fprintf(out, "    command: %s\n", yamlScalar(command))
		out.WriteString("    retry:\n      manual:\n        allowed: false\n")
		emitWorkflowCheck(out, "    ", pipeline.EventProvider, workflow, workflow.GroupKey, "", "Workflow could not be run", failure.Summary)
		_, _ = fmt.Fprintf(out, "    depends_on: %s\n", yamlScalar(pipeline.CompilerStep))
		out.WriteString("    checkout:\n      skip: true\n")
		return nil
	}
	if workflow.SkipReason != "" && workflow.Aggregate {
		_, _ = fmt.Fprintf(out, "  - label: %s\n", yamlScalar(":github: workflow · "+workflow.GroupLabel))
		_, _ = fmt.Fprintf(out, "    key: %s\n", yamlScalar(workflow.GroupKey))
		if workflow.Condition != "" {
			_, _ = fmt.Fprintf(out, "    if: %s\n", yamlScalar(workflow.Condition))
		}
		_, _ = fmt.Fprintf(out, "    skip: %s\n", yamlScalar(workflow.SkipReason))
		out.WriteString("    type: command\n")
		emitWorkflowCheck(out, "    ", pipeline.EventProvider, workflow, workflow.GroupKey, "", "", "")
		_, _ = fmt.Fprintf(out, "    depends_on: %s\n", yamlScalar(pipeline.CompilerStep))
		out.WriteString("    checkout:\n      skip: true\n")
		return nil
	}
	stepIndent := "  "
	if workflow.Grouped {
		groupLabel := workflow.GroupLabel
		if workflow.Aggregate {
			groupLabel = ":github: workflow · " + groupLabel
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
		if workflow.Aggregate {
			_, _ = fmt.Fprintf(out, "    depends_on: %s\n", yamlScalar(pipeline.CompilerStep))
		}
		out.WriteString("    steps:\n")
		stepIndent = "      "
	}
	attributeIndent := stepIndent + "  "
	if workflow.ConcurrencyGate != nil {
		// Keep each opening marker immediately before its dependency-blocked
		// closing marker. Their ordered queue positions hold the group before a
		// later build or sibling scope can enter it.
		dependencies := []dependency{{Step: pipeline.CompilerStep}}
		if workflow.Aggregate {
			dependencies = nil
		}
		emitConcurrencyGateStep(out, stepIndent, attributeIndent, ":github: Start workflow concurrency", workflow.GateOpenKey, workflow.ConcurrencyGate, dependencies)
		dependencies = make([]dependency, 0, len(workflow.Jobs)+len(workflow.ReusableConcurrencyGates)+1)
		if !workflow.Aggregate {
			dependencies = append(dependencies, dependency{Step: pipeline.CompilerStep})
		}
		for _, job := range workflow.Jobs {
			dependencies = append(dependencies, dependency{Step: job.Key, AllowFailure: true})
		}
		for _, gate := range workflow.ReusableConcurrencyGates {
			if gate.ParentID == "" {
				dependencies = append(dependencies, dependency{Step: gate.CloseKey, AllowFailure: true})
			}
		}
		emitConcurrencyGateStep(out, stepIndent, attributeIndent, ":github: Finish workflow concurrency", workflow.GateCloseKey, workflow.ConcurrencyGate, dependencies)
	}
	gateOpenKeys := make(map[string]string, len(workflow.ReusableConcurrencyGates))
	for _, gate := range workflow.ReusableConcurrencyGates {
		dependencies := reusableGateOpenDependencies(workflow, gate, gateOpenKeys, pipeline.CompilerStep)
		emitConcurrencyGateStep(out, stepIndent, attributeIndent, ":github: Start reusable-workflow concurrency", gate.OpenKey, &gate.ConcurrencyGate, dependencies)
		gateOpenKeys[gate.ID] = gate.OpenKey
		dependencies = make([]dependency, 0, len(gate.Members)+len(workflow.ReusableConcurrencyGates))
		for _, member := range gate.Members {
			dependencies = append(dependencies, dependency{Step: member, AllowFailure: true})
		}
		for _, child := range workflow.ReusableConcurrencyGates {
			if child.ParentID == gate.ID {
				dependencies = append(dependencies, dependency{Step: child.CloseKey, AllowFailure: true})
			}
		}
		emitConcurrencyGateStep(out, stepIndent, attributeIndent, ":github: Finish reusable-workflow concurrency", gate.CloseKey, &gate.ConcurrencyGate, dependencies)
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
		_, _ = fmt.Fprintf(out, "%s- label: %s\n", stepIndent, yamlScalar(":github: job · "+job.Label))
		_, _ = fmt.Fprintf(out, "%skey: %s\n", attributeIndent, yamlScalar(job.Key))
		if runtimeImage != "" {
			_, _ = fmt.Fprintf(out, "%simage: %s\n", attributeIndent, yamlScalar(runtimeImage))
		}
		commands := []string{
			"set -euo pipefail",
			"echo '~~~ :package: Prepare GitHub Actions runtime'",
			`bootstrap_exit() { bootstrap_status=$?; if [ "$bootstrap_status" -ne 0 ]; then echo "^^^ +++"; fi; if [ -n "${bootstrap_dir:-}" ]; then rm -rf -- "$bootstrap_dir" || true; fi; exit "$bootstrap_status"; }`,
			"trap bootstrap_exit EXIT",
			`bootstrap_dir="$(mktemp -d "${TMPDIR:-/tmp}/buildkite-gha.XXXXXXXX")"`,
			"buildkite-agent artifact download " + shellQuote(distributionPath) + ` "$bootstrap_dir" --step ` + shellQuote(pipeline.CompilerStep),
			"distribution=\"$bootstrap_dir/" + distributionPath + `"`,
			`if command -v sha256sum >/dev/null 2>&1; then actual_distribution_digest="$(sha256sum "$distribution" | awk '{print "sha256:" $1}')"; elif command -v shasum >/dev/null 2>&1; then actual_distribution_digest="$(shasum -a 256 "$distribution" | awk '{print "sha256:" $1}')"; else echo 'buildkite-gha: no SHA-256 tool available' >&2; exit 1; fi`,
			"test \"$actual_distribution_digest\" = " + shellQuote(distributionDigest),
			`chmod 0500 "$distribution"`,
		}
		experimentalRunnerUser := !pipeline.DisableRunnerUser && platform == "linux/amd64"
		runJob := `"$distribution" run-job --plan-digest ` + shellQuote(job.PlanDigest) + " --plan-producer " + shellQuote(pipeline.CompilerStep)
		if experimentalRunnerUser {
			planPath, err := PlanPath(job.PlanDigest)
			if err != nil {
				return fmt.Errorf("job %q: %w", job.Key, err)
			}
			commands = append(commands,
				"buildkite-agent artifact download "+shellQuote(planPath)+` "$bootstrap_dir" --step `+shellQuote(pipeline.CompilerStep),
				`plan="$bootstrap_dir/`+planPath+`"`,
				`if command -v sha256sum >/dev/null 2>&1; then actual_plan_digest="$(sha256sum "$plan" | awk '{print "sha256:" $1}')"; elif command -v shasum >/dev/null 2>&1; then actual_plan_digest="$(shasum -a 256 "$plan" | awk '{print "sha256:" $1}')"; else echo 'buildkite-gha: no SHA-256 tool available' >&2; exit 1; fi`,
				"test \"$actual_plan_digest\" = "+shellQuote(job.PlanDigest),
			)
			commands = append(commands, experimentalRunnerUserBootstrap(job.RequiresMise, runtimeImage != "", job.Cache)...)
			runJob = "BUILDKITE_GHA_PLAN_DIGEST=" + shellQuote(job.PlanDigest) + ` "$distribution" run-job --plan "$plan"`
		}
		if runtimeImage != "" {
			runJob += " --hosted-tool-cache"
		}
		if experimentalRunnerUser {
			runJob = experimentalRunnerUserCommand(runJob)
		}
		commands = append(commands, `trap 'rm -rf -- "$bootstrap_dir"' EXIT`, "unset -f bootstrap_exit", runJob)
		command := strings.Join(commands, "\n")
		_, _ = fmt.Fprintf(out, "%scommand: %s\n", attributeIndent, yamlScalar(command))
		if workflow.Aggregate {
			checkLabel := job.CheckLabel
			if checkLabel == "" {
				checkLabel = job.Label
			}
			emitWorkflowCheck(out, attributeIndent, pipeline.EventProvider, workflow, job.Key, checkLabel, "", "")
		}
		if job.Queue != "" {
			_, _ = fmt.Fprintf(out, "%sagents:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s  queue: %s\n", attributeIndent, yamlScalar(job.Queue))
		}
		_, _ = fmt.Fprintf(out, "%scheckout:\n%s  skip: true\n", attributeIndent, attributeIndent)
		cache := mergedCacheVolume(job.Cache, job.RequiresMise, platform)
		if cache != nil {
			_, _ = fmt.Fprintf(out, "%scache:\n", attributeIndent)
			_, _ = fmt.Fprintf(out, "%s  paths:\n", attributeIndent)
			for _, path := range cache.Paths {
				_, _ = fmt.Fprintf(out, "%s    - %s\n", attributeIndent, yamlScalar(path))
			}
			if cache.Name != "" {
				_, _ = fmt.Fprintf(out, "%s  name: %s\n", attributeIndent, yamlScalar(cache.Name))
			}
			if cache.Size != "" {
				_, _ = fmt.Fprintf(out, "%s  size: %s\n", attributeIndent, yamlScalar(cache.Size))
			}
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
		} else if workflow.GateOpenKey != "" || len(job.Dependencies) != 0 || len(job.ConcurrencyGates) != 0 {
			_, _ = fmt.Fprintf(out, "%sdepends_on:\n", attributeIndent)
		}
		if workflow.GateOpenKey != "" {
			_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: false\n", attributeIndent, yamlScalar(workflow.GateOpenKey), attributeIndent)
		}
		for _, dependency := range job.Dependencies {
			_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: true\n", attributeIndent, yamlScalar(dependency), attributeIndent)
		}
		for _, gate := range job.ConcurrencyGates {
			_, _ = fmt.Fprintf(out, "%s  - step: %s\n%s    allow_failure: false\n", attributeIndent, yamlScalar(gateOpenKeys[gate.ID]), attributeIndent)
		}
	}
	return nil
}

func prepareReusableConcurrencyGates(jobs []Job) ([]preparedConcurrencyGate, error) {
	var gates []preparedConcurrencyGate
	indexes := make(map[string]int)
	jobsByKey := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		jobsByKey[job.Key] = job
		parentID := ""
		seen := make(map[string]bool, len(job.ConcurrencyGates))
		for _, declared := range job.ConcurrencyGates {
			if declared.ID == "" || declared.Group == "" || len(declared.Group) > maxConcurrencyGroupLength {
				return nil, fmt.Errorf("job %q has invalid reusable-workflow concurrency gate", job.Key)
			}
			if seen[declared.ID] {
				return nil, fmt.Errorf("job %q repeats reusable-workflow concurrency gate %q", job.Key, declared.ID)
			}
			seen[declared.ID] = true
			index, exists := indexes[declared.ID]
			if !exists {
				index = len(gates)
				indexes[declared.ID] = index
				gates = append(gates, preparedConcurrencyGate{
					ConcurrencyGate: ConcurrencyGate{ID: declared.ID, Group: declared.Group, Queue: job.Queue},
					ParentID:        parentID,
				})
			} else if gates[index].Group != declared.Group || gates[index].ParentID != parentID {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q has inconsistent membership", declared.ID)
			}
			gates[index].Members = append(gates[index].Members, job.Key)
			parentID = declared.ID
		}
	}
	for _, gate := range gates {
		for parentID := gate.ParentID; parentID != ""; {
			parent := gates[indexes[parentID]]
			if parent.Group == gate.Group {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with enclosing gate %q", gate.ID, parent.ID)
			}
			parentID = parent.ParentID
		}
	}
	for _, gate := range gates {
		members := make(map[string]bool, len(gate.Members))
		for _, key := range gate.Members {
			job := jobsByKey[key]
			if job.Concurrency > 0 && job.ConcurrencyGroup == gate.Group {
				return nil, fmt.Errorf("reusable-workflow concurrency gate %q shares group with member job %q", gate.ID, key)
			}
			members[key] = true
		}
		for _, key := range gate.Members {
			for _, dependency := range jobsByKey[key].Dependencies {
				if !members[dependency] {
					return nil, fmt.Errorf("reusable-workflow concurrency gate %q has an external prerequisite", gate.ID)
				}
			}
		}
	}
	return gates, nil
}

func reusableGateOpenDependencies(workflow preparedWorkflow, gate preparedConcurrencyGate, openKeys map[string]string, compilerStep string) []dependency {
	seen := make(map[string]bool)
	var dependencies []dependency
	add := func(step string, allowFailure bool) {
		if step != "" && !seen[step] {
			seen[step] = true
			dependencies = append(dependencies, dependency{Step: step, AllowFailure: allowFailure})
		}
	}
	switch {
	case gate.ParentID != "":
		add(openKeys[gate.ParentID], false)
	case workflow.GateOpenKey != "":
		add(workflow.GateOpenKey, false)
	case !workflow.Aggregate:
		add(compilerStep, false)
	}
	return dependencies
}

func emitWorkflowCheck(out *bytes.Buffer, indent, provider string, workflow preparedWorkflow, checkKey, jobLabel, title, summary string) {
	_, _ = fmt.Fprintf(out, "%snotify:\n", indent)
	switch provider {
	case "github":
		_, _ = fmt.Fprintf(out, "%s  - github_check:\n", indent)
	case "cursor-origin":
		_, _ = fmt.Fprintf(out, "%s  - origin_check:\n", indent)
		_, _ = fmt.Fprintf(out, "%s      key: %s\n", indent, yamlScalar(checkKey))
	}
	checkName := workflow.CheckName
	if checkName == "" {
		checkName = workflow.GroupLabel
	}
	if jobLabel != "" {
		checkName += " / " + jobLabel
	}
	checkName += " (" + workflow.Event + ")"
	_, _ = fmt.Fprintf(out, "%s      name: %s\n", indent, yamlScalar(checkName))
	if title != "" {
		_, _ = fmt.Fprintf(out, "%s      output:\n", indent)
		_, _ = fmt.Fprintf(out, "%s        title: %s\n", indent, yamlScalar(title))
		_, _ = fmt.Fprintf(out, "%s        summary: %s\n", indent, yamlScalar(summary))
	}
}

func platformCacheKey(platform string) string {
	return strings.ReplaceAll(platform, "/", "-")
}

func platformMiseCachePath(platform string) string {
	root := runtimeCacheRoot
	if platform == "darwin/arm64" {
		root = darwinRuntimeCacheRoot
	}
	return root + "/mise/" + platformCacheKey(platform)
}

func mergedCacheVolume(configured *CacheVolume, requiresMise bool, platform string) *CacheVolume {
	if configured == nil && !requiresMise {
		return nil
	}
	cache := &CacheVolume{}
	if configured != nil {
		cache.Paths = append(cache.Paths, configured.Paths...)
		cache.Name = configured.Name
		cache.Size = configured.Size
	}
	if requiresMise {
		misePath := platformMiseCachePath(platform)
		found := false
		for _, path := range cache.Paths {
			if path == misePath {
				found = true
				break
			}
		}
		if !found {
			cache.Paths = append(cache.Paths, misePath)
		}
		if cache.Name == "" {
			cache.Name = runtimeCacheName + "-" + platformCacheKey(platform)
		}
	}
	return cache
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
	if job.Cache != nil {
		if err := ValidateCacheVolume(*job.Cache); err != nil {
			return fmt.Errorf("job %q has invalid cache configuration: %w", job.Key, err)
		}
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

func validArtifactPath(path string) bool {
	native := filepath.FromSlash(path)
	return path != "" && filepath.IsLocal(native) && filepath.ToSlash(filepath.Clean(native)) == path
}

func yamlScalar(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
