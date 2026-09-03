package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/git"
	"github.com/buildkite/buildkite-gha/internal/program"
)

const Schema = "https://buildkite.com/schemas/buildkite-gha/job-plan-v2.schema.json"

const MaxNeedProducers = 1024
const MaxNeedOutputs = 64
const MaxCallGuards = 4
const MaxEventPayloadBytes = 25 << 20

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var compilerVersionPattern = regexp.MustCompile(`^[ -~]{1,256}$`)
var targetPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
var logicalJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var actionLockIDPattern = regexp.MustCompile(`^a-[0-9a-f]{16}$`)
var containerEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var containerPortPattern = regexp.MustCompile(`^(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])(?::(?:[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?(?:/(?:tcp|udp))?$`)
var serviceNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,254}$`)
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var githubWorkflowFilenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.ya?ml$`)

// ValidContainerImageReference reports whether image is a supported literal
// Docker image reference.
func ValidContainerImageReference(image string) bool {
	return metadata.ValidDockerImageReference(image)
}

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

var githubWorkflowAccessTokenPermissionAccess = map[string]map[string]bool{
	"actions":           {"read": true, "write": true},
	"artifact_metadata": {"read": true, "write": true},
	"attestations":      {"read": true, "write": true},
	"checks":            {"read": true, "write": true},
	"contents":          {"read": true, "write": true},
	"deployments":       {"read": true, "write": true},
	"discussions":       {"read": true, "write": true},
	"issues":            {"read": true, "write": true},
	"packages":          {"read": true, "write": true},
	"pages":             {"read": true, "write": true},
	"pull_requests":     {"read": true, "write": true},
	"security_events":   {"read": true, "write": true},
	"statuses":          {"read": true, "write": true},
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
	DockerImage  string                    `json:"docker_image,omitempty"`
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
	Provider        string          `json:"provider"`
	Name            string          `json:"name"`
	PayloadDigest   string          `json:"payload_digest"`
	PayloadArtifact bool            `json:"payload_artifact,omitempty"`
	Payload         *map[string]any `json:"-"`
	Repository      string          `json:"repository,omitempty"`
	Ref             string          `json:"ref,omitempty"`
	HeadRef         string          `json:"head_ref,omitempty"`
	BaseRef         string          `json:"base_ref,omitempty"`
	SHA             string          `json:"sha,omitempty"`
	Actor           string          `json:"actor,omitempty"`
}

// EventRepositoryOwner returns the owner component of an event repository.
func EventRepositoryOwner(repository string) string {
	owner, _, _ := strings.Cut(repository, "/")
	return owner
}

// EventRefName returns the short branch, tag, or pull request ref name.
func EventRefName(ref string) string {
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/pull/"} {
		if name, ok := strings.CutPrefix(ref, prefix); ok {
			return name
		}
	}
	return ""
}

// EventRefType returns the GitHub ref type for a branch, tag, or pull request ref.
func EventRefType(ref string) string {
	if strings.HasPrefix(ref, "refs/tags/") {
		return "tag"
	}
	if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/pull/") {
		return "branch"
	}
	return ""
}

// EventServerURL returns the repository provider URL exposed through the
// GitHub-compatible expression context and environment.
func EventServerURL(provider string) string {
	switch provider {
	case "github":
		return "https://github.com"
	case "cursor-origin":
		return "https://origin.cursor.com"
	default:
		return ""
	}
}

type Workflow struct {
	Path string `json:"path"`
	// RunPath identifies the top-level caller when Path identifies a reusable
	// workflow. An empty RunPath means Path already identifies the workflow run.
	RunPath      string                `json:"run_path,omitempty"`
	Name         string                `json:"name,omitempty"`
	Digest       string                `json:"digest"`
	LogicalJobID string                `json:"logical_job_id"`
	Remote       *RemoteWorkflowSource `json:"remote,omitempty"`
}

// RemoteWorkflowSource binds a workflow file to one immutable public
// repository tree. Workflow.Digest binds the selected file bytes.
type RemoteWorkflowSource struct {
	Repository   string `json:"repository"`
	RequestedRef string `json:"requested_ref"`
	Commit       string `json:"commit"`
	SourceDigest string `json:"source_digest"`
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

// CallGuard is one immutable caller-scoped reusable-workflow condition. Needs
// is hydrated only from its independently verified producer manifests.
type CallGuard struct {
	Condition           string                   `json:"condition"`
	Inputs              map[string]any           `json:"inputs,omitempty"`
	DeferredInputs      map[string]DeferredInput `json:"deferred_inputs,omitempty"`
	DeferredInputValues map[string]any           `json:"-"`
	NeedSources         map[string][]NeedSource  `json:"need_sources,omitempty"`
	NeedOutputs         map[string][]NeedOutput  `json:"need_outputs,omitempty"`
	Needs               map[string]Need          `json:"-"`
}

type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type Span struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Container struct {
	Image string            `json:"image"`
	Env   map[string]string `json:"env,omitempty"`
	Ports []string          `json:"ports,omitempty"`
}

type ContainerCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type ServiceContainer struct {
	Image       string                `json:"image"`
	Credentials *ContainerCredentials `json:"credentials,omitempty"`
	Env         map[string]string     `json:"env,omitempty"`
	Ports       []string              `json:"ports,omitempty"`
	Volumes     []string              `json:"volumes,omitempty"`
	Options     string                `json:"options,omitempty"`
	Command     string                `json:"command,omitempty"`
	Entrypoint  string                `json:"entrypoint,omitempty"`
}

// DeferredInput binds one string workflow_call input to exact, verified
// prerequisite outputs. The runtime resolves it before evaluating callee fields.
type DeferredInput struct {
	Sources []NeedSource `json:"sources"`
	Outputs []NeedOutput `json:"outputs,omitempty"`
}

// GitHubToken describes one synthetic secrets.GITHUB_TOKEN value. Workflow is
// the top-level policy filename. Permissions are API-normalized and
// compiler-owned; the repository always comes from Event.
type GitHubToken struct {
	Workflow    string            `json:"workflow"`
	Permissions map[string]string `json:"permissions"`
	Aliases     []string          `json:"aliases,omitempty"`
}

// OIDCConfiguration applies additional Buildkite claims to every OIDC token
// minted for the job.
type OIDCConfiguration struct {
	Claims         []string `json:"claims,omitempty"`
	AWSSessionTags []string `json:"aws_session_tags,omitempty"`
	SubjectClaim   string   `json:"subject_claim,omitempty"`
}

// Job is the serialized runtime envelope for one compiler-selected workflow job
// instance. It contains the normalized Program plus instance metadata and the
// transport, authority, and runtime configuration needed to execute it.
type Job struct {
	Schema               string                   `json:"schema"`
	Compiler             Compiler                 `json:"compiler"`
	Runtime              *Runtime                 `json:"runtime,omitempty"`
	Workflow             Workflow                 `json:"workflow"`
	Event                Event                    `json:"event"`
	Target               Target                   `json:"target"`
	RequiredCapabilities []string                 `json:"required_capabilities"`
	RequiredSecrets      []string                 `json:"required_secrets,omitempty"`
	SecretMappings       map[string]string        `json:"secret_mappings,omitempty"`
	GitHubToken          *GitHubToken             `json:"github_token,omitempty"`
	IDTokenPermission    string                   `json:"id_token_permission,omitempty"`
	OIDC                 *OIDCConfiguration       `json:"oidc,omitempty"`
	Matrix               map[string]any           `json:"matrix,omitempty"`
	Inputs               map[string]any           `json:"inputs,omitempty"`
	DeferredInputs       map[string]DeferredInput `json:"deferred_inputs,omitempty"`
	DeferredInputValues  map[string]any           `json:"-"`
	Vars                 map[string]string        `json:"vars,omitempty"`
	Dependencies         []string                 `json:"dependencies,omitempty"`
	NeedSources          map[string][]NeedSource  `json:"need_sources,omitempty"`
	NeedOutputs          map[string][]NeedOutput  `json:"need_outputs,omitempty"`
	CallGuards           []CallGuard              `json:"call_guards,omitempty"`
	// Needs is populated only from verified producer-attributed manifests at
	// runtime. It is never accepted from or encoded into an immutable plan.
	Needs   map[string]Need  `json:"-"`
	Program *program.Program `json:"program"`
	// The executor-facing projections below are derived from Program. They are
	// never accepted from or encoded into the plan boundary.
	Env                     map[string]string `json:"-"`
	Condition               string            `json:"-"`
	ContinueOnError         bool              `json:"-"`
	TimeoutMinutes          float64           `json:"-"`
	DefaultShell            string            `json:"-"`
	DefaultWorkingDirectory string            `json:"-"`
	Outputs                 map[string]string `json:"-"`
	Actions                 []ActionLock      `json:"actions,omitempty"`
	// RequiresMise is the compiler's explicit action-runtime decision.
	RequiresMise       *bool                       `json:"requires_mise,omitempty"`
	Container          *Container                  `json:"-"`
	Services           map[string]ServiceContainer `json:"-"`
	ServiceOrder       []string                    `json:"-"`
	ServicesExpression string                      `json:"-"`
}

// NeedsMise reports whether a generated job needs the managed action runtime.
func (job Job) NeedsMise() bool {
	usesActions := len(job.Actions) != 0
	if executionJob := job.ExecutionJob(); executionJob != nil {
		for _, step := range executionJob.Steps {
			usesActions = usesActions || step.Invocation != nil || step.Kind == "uses"
		}
	}
	if !usesActions {
		return false
	}
	return job.RequiresMise == nil || *job.RequiresMise
}

// RuntimeDistributionDigest returns the executable digest bound to this plan.
func (job Job) RuntimeDistributionDigest() string {
	if job.Runtime != nil {
		return job.Runtime.DistributionDigest
	}
	return ""
}

// Decode rejects unknown fields and trailing JSON to stop on schema drift.
func Decode(source []byte) (Job, error) {
	if err := rejectDuplicateKeys(source); err != nil {
		return Job{}, fmt.Errorf("decode job plan: %w", err)
	}
	var presence struct {
		Program json.RawMessage `json:"program"`
	}
	if err := json.Unmarshal(source, &presence); err != nil {
		return Job{}, fmt.Errorf("decode job plan: %w", err)
	}
	if len(presence.Program) == 0 || string(presence.Program) == "null" {
		return Job{}, fmt.Errorf("decode job plan: normalized execution program is required")
	}
	if err := rejectAmbiguousProgramControls(presence.Program); err != nil {
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
	if err := job.ProjectProgram(); err != nil {
		return Job{}, fmt.Errorf("decode job plan: %w", err)
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

func rejectAmbiguousProgramControls(source json.RawMessage) error {
	var document struct {
		Job struct {
			Steps []map[string]json.RawMessage `json:"steps"`
		} `json:"job"`
	}
	if err := json.Unmarshal(source, &document); err != nil {
		return err
	}
	for i, step := range document.Job.Steps {
		for _, controlName := range []string{"continue_on_error", "timeout_minutes"} {
			var controlSource json.RawMessage
			matches := 0
			for name, value := range step {
				if strings.EqualFold(name, controlName) {
					matches++
					controlSource = value
				}
			}
			if matches > 1 {
				return fmt.Errorf("step %d repeats %s with different casing", i+1, strings.ReplaceAll(controlName, "_", "-"))
			}
			if len(controlSource) == 0 {
				continue
			}
			var control map[string]json.RawMessage
			if err := json.Unmarshal(controlSource, &control); err != nil {
				continue
			}
			hasLiteral, hasExpression := false, false
			literalCount, expressionCount := 0, 0
			for name := range control {
				if strings.EqualFold(name, "literal") {
					hasLiteral, literalCount = true, literalCount+1
				}
				if strings.EqualFold(name, "expression") {
					hasExpression, expressionCount = true, expressionCount+1
				}
			}
			if literalCount > 1 || expressionCount > 1 {
				return fmt.Errorf("step %d repeats a field in %s with different casing", i+1, strings.ReplaceAll(controlName, "_", "-"))
			}
			if hasLiteral && hasExpression {
				return fmt.Errorf("step %d has both literal and expression %s", i+1, strings.ReplaceAll(controlName, "_", "-"))
			}
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
	if err := job.ProjectProgram(); err != nil {
		return nil, err
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

// DecodeEventPayload verifies and decodes one immutable event artifact.
func DecodeEventPayload(source []byte, expectedDigest string) (map[string]any, error) {
	if len(source) > MaxEventPayloadBytes {
		return nil, fmt.Errorf("event payload artifact exceeds the %d-byte limit", MaxEventPayloadBytes)
	}
	digest := sha256.Sum256(source)
	if "sha256:"+hex.EncodeToString(digest[:]) != expectedDigest {
		return nil, fmt.Errorf("event payload artifact does not match its digest")
	}
	if err := rejectDuplicateKeys(source); err != nil {
		return nil, fmt.Errorf("decode event payload artifact: %w", err)
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode event payload artifact: %w", err)
	}
	if payload == nil {
		return nil, fmt.Errorf("event payload artifact must contain a JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode event payload artifact: multiple JSON values")
		}
		return nil, fmt.Errorf("decode event payload artifact: %w", err)
	}
	return payload, nil
}

func (job Job) Validate() error {
	if job.Schema != Schema {
		return fmt.Errorf("unsupported job plan schema %q", job.Schema)
	}
	if job.Program == nil {
		return fmt.Errorf("normalized execution program is required")
	}
	if err := job.Program.Validate(); err != nil {
		return fmt.Errorf("normalized execution program: %w", err)
	}
	executionJob := job.ExecutionJob()
	if job.RequiresMise == nil {
		return fmt.Errorf("job plan requires an explicit requires_mise decision")
	}
	if job.Runtime == nil || !digestPattern.MatchString(job.Runtime.DistributionDigest) {
		return fmt.Errorf("job plan runtime distribution digest is required")
	}
	if job.RequiresMise != nil && !*job.RequiresMise {
		for _, step := range executionJob.Steps {
			if step.Invocation != nil && step.Invocation.Lock == "" {
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
	if len(job.Event.Repository) > 512 || len(job.Event.Ref) > 1024 || len(job.Event.HeadRef) > 1024 || len(job.Event.BaseRef) > 1024 || len(job.Event.SHA) > 128 || len(job.Event.Actor) > 256 {
		return fmt.Errorf("job plan event identity exceeds its size limit")
	}
	if job.Event.Payload != nil {
		payload, err := json.Marshal(job.Event.Payload)
		if err != nil {
			return fmt.Errorf("encode job plan event payload: %w", err)
		}
		if len(payload) > MaxEventPayloadBytes {
			return fmt.Errorf("job plan event payload exceeds the %d-byte limit", MaxEventPayloadBytes)
		}
		digest := sha256.Sum256(payload)
		if "sha256:"+hex.EncodeToString(digest[:]) != job.Event.PayloadDigest {
			return fmt.Errorf("job plan event payload does not match its digest")
		}
	}
	if job.Workflow.Path == "" || len(job.Workflow.Path) > 1024 || !utf8.ValidString(job.Workflow.Path) || hasControl(job.Workflow.Path) || !digestPattern.MatchString(job.Workflow.Digest) || job.Workflow.LogicalJobID == "" {
		return fmt.Errorf("job plan requires a workflow path, sha256 digest, and logical job id")
	}
	if job.Workflow.RunPath != "" && !validWorkflowRunPath(job.Workflow.RunPath) {
		return fmt.Errorf("job plan has invalid workflow run path")
	}
	if err := validateRemoteWorkflowSource(job.Workflow); err != nil {
		return err
	}
	if len(job.Workflow.Name) > 1024 {
		return fmt.Errorf("job plan workflow name exceeds its size limit")
	}
	if !targetPattern.MatchString(job.Target.StepKey) {
		return fmt.Errorf("job plan requires a target step key")
	}
	if job.Target.Queue != "" && !targetPattern.MatchString(job.Target.Queue) {
		return fmt.Errorf("job plan has invalid target queue %q", job.Target.Queue)
	}
	if job.TimeoutMinutes < 0 || job.TimeoutMinutes > 360 {
		return fmt.Errorf("job timeout_minutes must be between 0 and 360")
	}
	if len(job.Condition) > 65536 || len(job.RequiredSecrets) > 128 || len(job.SecretMappings) > 128 {
		return fmt.Errorf("job plan condition, required secrets, or secret mappings exceed their size limit")
	}
	if err := validateInputs(job.Inputs); err != nil {
		return err
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
	if len(job.ServiceOrder) != 0 && len(job.Services) == 0 {
		return fmt.Errorf("job plan service order requires static services")
	}
	if job.Container != nil || len(job.Services) != 0 || job.ServicesExpression != "" {
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
		if len(job.ServiceOrder) != len(job.Services) {
			return fmt.Errorf("job plan service order must name every static service")
		}
		seen := make(map[string]bool, len(job.ServiceOrder))
		for _, name := range job.ServiceOrder {
			if _, ok := job.Services[name]; !ok || seen[name] {
				return fmt.Errorf("job plan service order contains unknown or repeated service %q", name)
			}
			seen[name] = true
		}
		for name, service := range job.Services {
			if !serviceNamePattern.MatchString(name) {
				return fmt.Errorf("service name %q must be lowercase and valid", name)
			}
			if err := validateServiceContainer(service, true); err != nil {
				return fmt.Errorf("service %q: %w", name, err)
			}
		}
		if len(job.ServicesExpression) > 65536 {
			return fmt.Errorf("services expression exceeds its size limit")
		}
	}
	if !sort.StringsAreSorted(job.RequiredSecrets) {
		return fmt.Errorf("job plan required secrets must be sorted")
	}
	seenRequiredSecrets := make(map[string]struct{}, len(job.RequiredSecrets))
	for i, name := range job.RequiredSecrets {
		if !secretNamePattern.MatchString(name) || i > 0 && job.RequiredSecrets[i-1] == name {
			return fmt.Errorf("job plan contains invalid or repeated required secret %q", name)
		}
		if strings.EqualFold(name, "GITHUB_TOKEN") {
			return fmt.Errorf("job plan must provide GITHUB_TOKEN through the scoped workflow token contract")
		}
		normalized := strings.ToUpper(name)
		if _, exists := seenRequiredSecrets[normalized]; exists {
			return fmt.Errorf("job plan repeats case-insensitive required secret %q", name)
		}
		seenRequiredSecrets[normalized] = struct{}{}
	}
	if len(job.RequiredSecrets) != 0 {
		if _, ok := capabilities["secrets"]; !ok {
			return fmt.Errorf("job plan required secrets need the secrets capability")
		}
	}
	requiredSecrets := make(map[string]struct{}, len(job.RequiredSecrets))
	ordinaryAliases := make(map[string]struct{}, max(len(job.RequiredSecrets), len(job.SecretMappings)))
	for _, name := range job.RequiredSecrets {
		requiredSecrets[name] = struct{}{}
		if len(job.SecretMappings) == 0 {
			ordinaryAliases[strings.ToUpper(name)] = struct{}{}
		}
	}
	for alias, source := range job.SecretMappings {
		if !secretNamePattern.MatchString(alias) || strings.EqualFold(alias, "GITHUB_TOKEN") || !secretNamePattern.MatchString(source) {
			return fmt.Errorf("job plan contains invalid secret mapping %q to %q", alias, source)
		}
		if _, ok := requiredSecrets[source]; !ok {
			return fmt.Errorf("job plan secret mapping %q references undeclared source %q", alias, source)
		}
		normalized := strings.ToUpper(alias)
		if _, exists := ordinaryAliases[normalized]; exists {
			return fmt.Errorf("job plan repeats case-insensitive secret alias %q", alias)
		}
		ordinaryAliases[normalized] = struct{}{}
	}
	if len(job.SecretMappings) != 0 {
		if _, ok := capabilities["secrets"]; !ok {
			return fmt.Errorf("job plan secret mappings need the secrets capability")
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
		if err := ValidateGitHubWorkflowPolicyFilename(job.GitHubToken.Workflow); err != nil {
			return err
		}
		if err := validateGitHubTokenPermissions(job.GitHubToken.Permissions); err != nil {
			return err
		}
		if !sort.StringsAreSorted(job.GitHubToken.Aliases) {
			return fmt.Errorf("job plan GitHub token aliases must be sorted")
		}
		for i, alias := range job.GitHubToken.Aliases {
			if !secretNamePattern.MatchString(alias) || strings.EqualFold(alias, "GITHUB_TOKEN") || i > 0 && strings.EqualFold(job.GitHubToken.Aliases[i-1], alias) {
				return fmt.Errorf("job plan contains invalid or repeated GitHub token alias %q", alias)
			}
			if _, exists := ordinaryAliases[strings.ToUpper(alias)]; exists {
				return fmt.Errorf("job plan GitHub token alias %q overlaps ordinary secret authority", alias)
			}
		}
	}
	if job.IDTokenPermission != "" && job.IDTokenPermission != "read" && job.IDTokenPermission != "write" {
		return fmt.Errorf("job plan contains invalid id-token permission %q", job.IDTokenPermission)
	}
	if job.OIDC != nil {
		for i, claim := range job.OIDC.Claims {
			if strings.TrimSpace(claim) == "" {
				return fmt.Errorf("job plan OIDC claims entry %d must be a non-empty string", i)
			}
		}
		for i, claim := range job.OIDC.AWSSessionTags {
			if strings.TrimSpace(claim) == "" {
				return fmt.Errorf("job plan OIDC AWS session tags entry %d must be a non-empty string", i)
			}
		}
		if job.OIDC.SubjectClaim != "" && strings.TrimSpace(job.OIDC.SubjectClaim) == "" {
			return fmt.Errorf("job plan OIDC subject claim must be a non-empty string")
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
		if len(name) > 255 || !logicalJobIDPattern.MatchString(name) || len(sources) == 0 || len(sources) > MaxNeedProducers {
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
	deferredDependencies, err := validateDeferredInputs(job.Inputs, job.DeferredInputs, dependencies)
	if err != nil {
		return fmt.Errorf("job plan deferred inputs: %w", err)
	}
	for dependency := range deferredDependencies {
		sourcedDependencies[dependency] = struct{}{}
	}
	if len(job.CallGuards) > MaxCallGuards {
		return fmt.Errorf("job plan has more than %d reusable-workflow call guards", MaxCallGuards)
	}
	if len(job.CallGuards) != len(executionJob.Guards) {
		return fmt.Errorf("job plan call guard projection does not match normalized program")
	}
	for i, guard := range job.CallGuards {
		if strings.TrimSpace(guard.Condition) == "" || len(guard.Condition) > 65536 {
			return fmt.Errorf("job plan call guard %d has an invalid condition", i+1)
		}
		if err := validateInputs(guard.Inputs); err != nil {
			return fmt.Errorf("job plan call guard %d: %w", i+1, err)
		}
		guardDependencies, err := validateCallGuardNeeds(guard, dependencies)
		if err != nil {
			return fmt.Errorf("job plan call guard %d: %w", i+1, err)
		}
		for dependency := range guardDependencies {
			sourcedDependencies[dependency] = struct{}{}
		}
		guardInputDependencies, err := validateDeferredInputs(guard.Inputs, guard.DeferredInputs, dependencies)
		if err != nil {
			return fmt.Errorf("job plan call guard %d deferred inputs: %w", i+1, err)
		}
		for dependency := range guardInputDependencies {
			sourcedDependencies[dependency] = struct{}{}
		}
	}
	if len(sourcedDependencies) != len(dependencies) {
		return fmt.Errorf("job plan dependencies and prerequisite producers differ")
	}
	if len(job.Actions) != 0 || hasStepActions(executionJob.Steps) {
		for _, lock := range job.Actions {
			if lock.DockerImage == "" {
				continue
			}
			if _, ok := capabilities["docker"]; !ok {
				return fmt.Errorf("prebuilt Docker actions require docker capability")
			}
			if _, ok := capabilities["network"]; !ok {
				return fmt.Errorf("prebuilt Docker actions require network capability")
			}
		}
		if err := validateActionLocks(job); err != nil {
			return err
		}
	}
	return nil
}

func validateRemoteWorkflowSource(workflow Workflow) error {
	if workflow.Remote == nil {
		return nil
	}
	remote := workflow.Remote
	if remote.Repository == "" || remote.Repository != strings.ToLower(remote.Repository) || len(remote.Repository) > 140 || remote.RequestedRef == "" || len(remote.RequestedRef) > 1024 || !utf8.ValidString(remote.RequestedRef) || hasControl(remote.RequestedRef) || !git.ValidObjectID(remote.Commit) || !digestPattern.MatchString(remote.SourceDigest) {
		return fmt.Errorf("job plan remote workflow has invalid immutable source provenance")
	}
	ref, err := source.Parse(workflow.Path)
	if err != nil || ref.Owner+"/"+ref.Repository != remote.Repository || ref.Ref != remote.RequestedRef || path.Dir(ref.Path) != ".github/workflows" || path.Ext(ref.Path) != ".yml" && path.Ext(ref.Path) != ".yaml" {
		return fmt.Errorf("job plan remote workflow path does not match source provenance")
	}
	repositoryRef, err := source.Parse(remote.Repository + "@x")
	if err != nil || strings.ToLower(repositoryRef.Owner+"/"+repositoryRef.Repository) != remote.Repository {
		return fmt.Errorf("job plan remote workflow has invalid canonical repository")
	}
	return nil
}

func validateInputs(inputs map[string]any) error {
	if len(inputs) > 25 {
		return fmt.Errorf("job plan inputs exceed their size limit")
	}
	for name, value := range inputs {
		if name == "" || len(name) > 255 {
			return fmt.Errorf("job plan has invalid input name")
		}
		switch value := value.(type) {
		case string:
			if len(value) > 65536 {
				return fmt.Errorf("job plan input %q exceeds its size limit", name)
			}
		case bool, json.Number, int, int64, uint64, float64:
		default:
			return fmt.Errorf("job plan input %q has unsupported type %T", name, value)
		}
	}
	return nil
}

func validateDeferredInputs(inputs map[string]any, deferred map[string]DeferredInput, dependencies map[string]struct{}) (map[string]struct{}, error) {
	if len(inputs)+len(deferred) > 25 {
		return nil, fmt.Errorf("inputs exceed their size limit")
	}
	names := make(map[string]struct{}, len(inputs)+len(deferred))
	for name := range inputs {
		names[strings.ToLower(name)] = struct{}{}
	}
	sourced := make(map[string]struct{})
	for name, input := range deferred {
		lowerName := strings.ToLower(name)
		if name == "" || len(name) > 255 {
			return nil, fmt.Errorf("has invalid input name")
		}
		if _, exists := names[lowerName]; exists {
			return nil, fmt.Errorf("repeats input %q", name)
		}
		names[lowerName] = struct{}{}
		if len(input.Sources) == 0 || len(input.Sources) > MaxNeedProducers {
			return nil, fmt.Errorf("input %q has no valid producers", name)
		}
		producers := make(map[string]struct{}, len(input.Sources))
		for i, source := range input.Sources {
			if !targetPattern.MatchString(source.StepKey) || !digestPattern.MatchString(source.PlanDigest) || i > 0 && input.Sources[i-1].StepKey >= source.StepKey {
				return nil, fmt.Errorf("input %q has invalid, repeated, or unsorted producer identity", name)
			}
			key := strings.ToLower(source.StepKey)
			if _, exists := dependencies[key]; !exists {
				return nil, fmt.Errorf("input %q producer %q is not a dependency", name, source.StepKey)
			}
			producers[key] = struct{}{}
			sourced[key] = struct{}{}
		}
		if len(input.Outputs) > MaxNeedOutputs {
			return nil, fmt.Errorf("input %q has too many output projections", name)
		}
		for i, output := range input.Outputs {
			if output.Name != "value" || !targetPattern.MatchString(output.StepKey) || !targetPattern.MatchString(output.Output) {
				return nil, fmt.Errorf("input %q has invalid output projection", name)
			}
			if _, exists := producers[strings.ToLower(output.StepKey)]; !exists {
				return nil, fmt.Errorf("input %q output selects unknown producer %q", name, output.StepKey)
			}
			if i > 0 && compareNeedOutput(input.Outputs[i-1], output) >= 0 {
				return nil, fmt.Errorf("input %q output projections must be unique and sorted", name)
			}
		}
	}
	return sourced, nil
}

func validateCallGuardNeeds(guard CallGuard, dependencies map[string]struct{}) (map[string]struct{}, error) {
	sourced := make(map[string]struct{})
	names := make(map[string]map[string]struct{}, len(guard.NeedSources))
	for name, sources := range guard.NeedSources {
		if len(name) > 255 || !logicalJobIDPattern.MatchString(name) || len(sources) == 0 || len(sources) > MaxNeedProducers {
			return nil, fmt.Errorf("contains invalid prerequisite %q", name)
		}
		lowerName := strings.ToLower(name)
		if _, exists := names[lowerName]; exists {
			return nil, fmt.Errorf("contains duplicate prerequisite %q", name)
		}
		steps := make(map[string]struct{}, len(sources))
		for i, source := range sources {
			if !targetPattern.MatchString(source.StepKey) || !digestPattern.MatchString(source.PlanDigest) || i > 0 && sources[i-1].StepKey >= source.StepKey {
				return nil, fmt.Errorf("prerequisite %q has invalid, repeated, or unsorted producer identity", name)
			}
			key := strings.ToLower(source.StepKey)
			if _, exists := dependencies[key]; !exists {
				return nil, fmt.Errorf("prerequisite %q producer %q is not a dependency", name, source.StepKey)
			}
			if _, exists := sourced[key]; exists {
				return nil, fmt.Errorf("dependency %q has multiple logical owners", source.StepKey)
			}
			steps[key] = struct{}{}
			sourced[key] = struct{}{}
		}
		names[lowerName] = steps
	}
	seenOutputNeeds := make(map[string]struct{}, len(guard.NeedOutputs))
	for name, outputs := range guard.NeedOutputs {
		lowerName := strings.ToLower(name)
		producers, exists := names[lowerName]
		if !exists {
			return nil, fmt.Errorf("prerequisite output projection %q has no matching prerequisite", name)
		}
		if _, exists := seenOutputNeeds[lowerName]; exists || len(outputs) > MaxNeedOutputs {
			return nil, fmt.Errorf("prerequisite %q has duplicate or excessive output projections", name)
		}
		seenOutputNeeds[lowerName] = struct{}{}
		for i, output := range outputs {
			if !targetPattern.MatchString(output.Name) || !targetPattern.MatchString(output.StepKey) || !targetPattern.MatchString(output.Output) {
				return nil, fmt.Errorf("prerequisite %q has invalid output projection", name)
			}
			if _, exists := producers[strings.ToLower(output.StepKey)]; !exists {
				return nil, fmt.Errorf("prerequisite %q output %q selects unknown producer %q", name, output.Name, output.StepKey)
			}
			if i > 0 && compareNeedOutput(outputs[i-1], output) >= 0 {
				return nil, fmt.Errorf("prerequisite %q output projections must be unique and sorted", name)
			}
		}
	}
	return sourced, nil
}

func validateServiceContainer(service ServiceContainer, templates bool) error {
	if !templates && !ValidContainerImageReference(service.Image) {
		return fmt.Errorf("invalid image reference")
	}
	if err := validateContainerImageEnv(service.Image, service.Env); err != nil {
		return err
	}
	if len(service.Ports) > 128 || len(service.Volumes) > 128 || len(service.Options) > 65536 || len(service.Command) > 65536 || len(service.Entrypoint) > 4096 {
		return fmt.Errorf("service container fields exceed their size limit")
	}
	for _, values := range [][]string{service.Ports, service.Volumes} {
		seen := map[string]bool{}
		for _, value := range values {
			if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") || seen[value] {
				return fmt.Errorf("service container has invalid or repeated value %q", value)
			}
			seen[value] = true
		}
	}
	for _, value := range []string{service.Options, service.Command, service.Entrypoint} {
		if strings.ContainsAny(value, "\x00\r") {
			return fmt.Errorf("service container field contains a control character")
		}
	}
	if service.Credentials != nil {
		if len(service.Credentials.Username) > 65536 || len(service.Credentials.Password) > 65536 || strings.ContainsAny(service.Credentials.Username, "\x00\r\n") || strings.ContainsAny(service.Credentials.Password, "\x00\r\n") {
			return fmt.Errorf("service container has invalid credentials")
		}
	}
	values := append(append(append([]string{service.Image, service.Options, service.Command, service.Entrypoint}, service.Ports...), service.Volumes...), mapStringValues(service.Env)...)
	if !templates {
		for _, value := range values {
			if strings.Contains(value, "${{") {
				return fmt.Errorf("service container retains a runtime template")
			}
		}
		if service.Credentials != nil && (strings.Contains(service.Credentials.Username, "${{") || strings.Contains(service.Credentials.Password, "${{")) {
			return fmt.Errorf("service container credentials retain a runtime template")
		}
	}
	return nil
}

func ValidateServiceContainer(service ServiceContainer) error {
	return validateServiceContainer(service, true)
}

func ValidateEvaluatedServiceContainer(service ServiceContainer) error {
	return validateServiceContainer(service, false)
}

func ValidateServiceName(name string) bool {
	return serviceNamePattern.MatchString(name)
}

func mapStringValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func validGitHubRepository(repository string) bool {
	if len(repository) > 140 || !githubRepositoryPattern.MatchString(repository) {
		return false
	}
	parts := strings.Split(repository, "/")
	return parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".."
}

func validWorkflowRunPath(value string) bool {
	if len(value) > 1024 || !utf8.ValidString(value) || hasControl(value) || strings.Contains(value, `\`) {
		return false
	}
	relative := strings.TrimPrefix(value, "./")
	return relative != "" && relative != "." && !path.IsAbs(relative) && path.Clean(relative) == relative && relative != ".." && !strings.HasPrefix(relative, "../")
}

// GitHubWorkflowPolicyFilename derives the workflow-policy endpoint filename
// from a compiler-owned repository workflow path.
func GitHubWorkflowPolicyFilename(path string) (string, error) {
	const prefix = ".github/workflows/"

	relative := strings.TrimPrefix(path, "./")
	if !strings.HasPrefix(relative, prefix) {
		return "", fmt.Errorf("GitHub workflow token requires a workflow directly under .github/workflows")
	}
	filename := strings.TrimPrefix(relative, prefix)
	if err := ValidateGitHubWorkflowPolicyFilename(filename); err != nil {
		return "", err
	}
	return filename, nil
}

// ValidateGitHubWorkflowPolicyFilename applies the Agent API endpoint's
// basename contract.
func ValidateGitHubWorkflowPolicyFilename(filename string) error {
	if len(filename) == 0 || len(filename) > 255 || strings.ContainsAny(filename, `/\\`) || !githubWorkflowFilenamePattern.MatchString(filename) {
		return fmt.Errorf("GitHub workflow token requires a simple .yml or .yaml filename of at most 255 bytes")
	}
	return nil
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

// ValidateGitHubWorkflowAccessTokenPermissions applies the workflow-policy
// endpoint's current server-maintained allowlist.
func ValidateGitHubWorkflowAccessTokenPermissions(permissions map[string]string) error {
	if len(permissions) == 0 || len(permissions) > len(githubWorkflowAccessTokenPermissionAccess) {
		return fmt.Errorf("GitHub workflow access token requires a non-empty bounded permission set")
	}
	for name, access := range permissions {
		allowed, ok := githubWorkflowAccessTokenPermissionAccess[name]
		if !ok || !allowed[access] {
			return fmt.Errorf("GitHub workflow access token contains unsupported permission %q with access %q", name, access)
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
	if !ValidContainerImageReference(image) {
		return fmt.Errorf("invalid image reference")
	}
	if err := validateContainerImageEnv(image, env); err != nil {
		return err
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

func validateContainerImageEnv(image string, env map[string]string) error {
	if len(image) == 0 || len(image) > 512 || !strings.Contains(image, "${{") && !ValidContainerImageReference(image) {
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
	return nil
}

func hasStepActions(steps []program.Step) bool {
	for _, step := range steps {
		if step.Invocation != nil && step.Invocation.Lock != "" {
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
		if lock.DockerImage != "" && !ValidContainerImageReference(lock.DockerImage) {
			return fmt.Errorf("action lock %q has invalid Docker image", lock.ID)
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
	for _, step := range job.ExecutionJob().Steps {
		if step.Kind != "uses" {
			if step.Invocation != nil && step.Invocation.Lock != "" {
				return fmt.Errorf("non-action step %q has an action selector", step.ID)
			}
			continue
		}
		if step.Invocation == nil || step.Invocation.Lock == "" {
			return fmt.Errorf("action step %q has no action selector", step.ID)
		}
		if !actionLockIDPattern.MatchString(step.Invocation.Lock) {
			return fmt.Errorf("action step %q has malformed action selector lock %q", step.ID, step.Invocation.Lock)
		}
		lock, ok := locks[step.Invocation.Lock]
		if !ok {
			return fmt.Errorf("action selector references missing lock %q", step.Invocation.Lock)
		}
		if err := validateTopLevelIdentity(step.Invocation.Uses.Source, lock); err != nil {
			return fmt.Errorf("action step %q: %w", step.ID, err)
		}
		if _, err := visit(lock.ID, 1); err != nil {
			return err
		}
	}
	if len(reachable) != len(locks) {
		return fmt.Errorf("job plan contains unused action locks")
	}
	for id, action := range job.Program.Actions {
		lock, ok := locks[id]
		if !ok || !reachable[id] {
			return fmt.Errorf("action program %q has no reachable immutable lock", id)
		}
		if action.Runtime == "docker" {
			image, _ := metadata.DockerImageReference(action.Image)
			if image != lock.DockerImage {
				return fmt.Errorf("action program %q Docker image does not match its immutable lock", id)
			}
		}
		for i, step := range action.Steps {
			if step.Invocation == nil {
				continue
			}
			selector, ok := lock.Children[step.Invocation.Uses.Source]
			if !ok || selector.Lock != step.Invocation.Lock {
				return fmt.Errorf("action program %q composite step %d does not match its immutable child lock", id, i+1)
			}
		}
	}
	for _, lock := range job.Actions {
		id := lock.ID
		if _, ok := job.Program.Actions[id]; ok {
			continue
		}
		if _, native, err := integration.AdmitNativeAdapter(integration.Identity{Source: lock.Source, Repository: lock.Repository, Path: lock.Path}, lock.Commit); err != nil {
			return fmt.Errorf("action lock %q is not an admitted native-adapter release: %w", id, err)
		} else if native {
			continue
		}
		return fmt.Errorf("reachable action lock %q has no normalized execution program", id)
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
		if lock.Repository == "" || len(lock.Repository) > 140 || lock.Repository != strings.ToLower(lock.Repository) || lock.RequestedRef == "" || len(lock.RequestedRef) > 1024 || !utf8.ValidString(lock.RequestedRef) || hasControl(lock.RequestedRef) || !git.ValidObjectID(lock.Commit) || lock.Path != "" && !cleanActionPath(lock.Path) {
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
	if after, ok := strings.CutPrefix(uses, "./"); ok {
		path := after
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
	if after, ok := strings.CutPrefix(uses, "./"); ok {
		path := after
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
	for segment := range strings.SplitSeq(value, "/") {
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

func (job Job) HasCapability(name string) bool {
	return slices.Contains(job.RequiredCapabilities, name)
}
