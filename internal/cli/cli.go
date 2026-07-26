// Package cli implements the buildkite-gha command-line interface.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	gharuntime "github.com/buildkite/buildkite-gha/internal/runtime"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

const usage = `Usage:
  buildkite-gha <command> [arguments]
  buildkite-gha --version

Commands:
  validate  Validate the supported static workflow subset
  compile   Compile a workflow to deterministic Buildkite pipeline YAML
  upload    Compile and upload a Buildkite pipeline
  run-job   Run a compiled job plan

Run "buildkite-gha help <command>" for command help.
`

var commandUsage = map[string]string{
	"validate": "Usage: buildkite-gha validate [--event-path <path>] [--profile hosted-tokenless] [--format text|json] <workflow>\n",
	"compile":  "Usage: buildkite-gha compile --event-path <path> [--format pipeline|ir-json] <workflow>\n",
	"upload":   "Usage: buildkite-gha upload [--event-path <path>] --runtime-queue hosted <workflow>\n",
	"run-job":  "Usage: buildkite-gha run-job --plan <path> [--result <path>]\n",
}

const (
	resultPublicationTimeout = 10 * time.Second
	unprivilegedRuntimeQueue = "hosted"
	hostedTokenlessProfile   = "hosted-tokenless"
)

// Run executes the command and returns its process exit code.
func Run(args []string, stdout, stderr io.Writer, version string) int {
	return run(args, stdout, stderr, version, transport.CommandRunner{Stderr: stderr})
}

func run(args []string, stdout, stderr io.Writer, version string, agentRunner transport.Runner) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	if args[0] == gharuntime.ContainerProcessHelperCommand {
		return gharuntime.RunContainerProcessHelper(args[1:])
	}

	switch args[0] {
	case "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	case "help":
		return help(args[1:], stdout, stderr)
	case "-v", "--version", "version":
		if len(args) != 1 {
			return usageError(stderr, "%s does not accept arguments", args[0])
		}
		_, _ = fmt.Fprintf(stdout, "buildkite-gha %s\n", version)
		return 0
	default:
		if commandHelp, ok := commandUsage[args[0]]; ok {
			if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
				_, _ = fmt.Fprint(stdout, commandHelp)
				if args[0] == "compile" {
					_, _ = fmt.Fprint(stdout, "\nPipeline output references content-addressed plans; compile does not materialize or upload those artifacts.\n")
				}
				if args[0] == "validate" {
					_, _ = fmt.Fprint(stdout, "\nThe hosted-tokenless profile resolves actions and applies production upload policy without executing jobs or proving arbitrary action runtime compatibility.\n")
				}
				if args[0] == "upload" {
					_, _ = fmt.Fprint(stdout, "\nThis unsigned, unprivileged path accepts an explicit event file or derives compatibility data from Buildkite; neither grants protected authority.\n")
				}
				return 0
			}
			switch args[0] {
			case "validate":
				return validate(args[1:], stdout, stderr, version)
			case "compile":
				return compile(args[1:], stdout, stderr, version)
			case "upload":
				return upload(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			case "run-job":
				return runJob(args[1:], stdout, stderr, version, transport.Agent{Runner: agentRunner})
			default:
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: %s: not implemented\n", args[0])
				return 1
			}
		}

		return usageError(stderr, "unknown command %q", args[0])
	}
}

func help(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) != 1 {
		return usageError(stderr, "help accepts at most one command")
	}

	commandHelp, ok := commandUsage[args[0]]
	if !ok {
		return usageError(stderr, "unknown command %q", args[0])
	}

	_, _ = fmt.Fprint(stdout, commandHelp)
	if args[0] == "validate" {
		_, _ = fmt.Fprint(stdout, "\nThe hosted-tokenless profile resolves actions and applies production upload policy without executing jobs or proving arbitrary action runtime compatibility.\n")
	}
	if args[0] == "compile" {
		_, _ = fmt.Fprint(stdout, "\nPipeline output references content-addressed plans; compile does not materialize or upload those artifacts.\n")
	}
	if args[0] == "upload" {
		_, _ = fmt.Fprint(stdout, "\nThis is the unsigned, unprivileged event-file path; it does not grant production plan authority.\n")
	}
	return 0
}

func runJob(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJobContext(ctx, args, stdout, stderr, version, agent)
}

func runJobContext(ctx context.Context, args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	planPath, resultPath, err := runJobArgs(args)
	if err != nil {
		return usageError(stderr, "run-job: %v", err)
	}
	source, err := os.ReadFile(planPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	planDigest := transport.Digest(source)
	if expected := os.Getenv("BUILDKITE_GHA_PLAN_DIGEST"); expected != "" {
		if planDigest != expected {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: plan digest %q does not match expected digest %q\n", planDigest, expected)
			return 1
		}
	}
	job, err := plan.Decode(source)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	if job.Compiler.Version != version {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: plan compiler version %q does not match runtime version %q\n", job.Compiler.Version, version)
		return 1
	}
	if err := verifyBuildkiteTarget(job); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	producer, publish, err := resultProducer(job, planDigest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", err)
		return 1
	}
	var artifactRoot string
	if publish {
		artifactRoot, err = os.MkdirTemp("", "buildkite-gha-results-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: create result artifact root: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(artifactRoot) }()
	}
	var actionMaterializer gharuntime.ActionMaterializer
	if (job.Schema == plan.SchemaV3 || job.Schema == plan.SchemaV4) && hasGitHubActionLocks(job.Actions) {
		actionCache, err := os.MkdirTemp("", "buildkite-gha-actions-")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: create action cache: %v\n", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(actionCache) }()
		store, err := actionsource.NewStore(actionCache, nil)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: configure action cache: %v\n", err)
			return 1
		}
		actionMaterializer = store
	}
	runner := gharuntime.Runner{
		Stdout:   stdout,
		Stderr:   stderr,
		Docker:   os.Getenv("BUILDKITE_GHA_DOCKER"),
		Git:      os.Getenv("BUILDKITE_GHA_GIT"),
		Secrets:  gharuntime.EnvironmentSecrets{},
		Redactor: gharuntime.AgentRedactor{Executable: os.Getenv("BUILDKITE_GHA_AGENT")},
		Actions:  actionMaterializer,
	}
	runner.RuntimeExecutable, err = os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: resolve runtime executable: %v\n", err)
		return 1
	}
	var result gharuntime.JobResult
	var runErr error
	if len(job.NeedSources) != 0 {
		job.Needs, runErr = gharuntime.ResolveNeeds(ctx, agent, artifactRoot, producer.BuildID, job.NeedSources)
		if runErr != nil {
			runErr = fmt.Errorf("hydrate prerequisite results: %w", runErr)
		}
	}
	if runErr == nil {
		result, runErr = runner.RunJob(ctx, job, "")
	}
	if result.Conclusion == "" {
		result.Conclusion = terminalErrorConclusion(ctx)
	}
	if resultPath != "" && result.Conclusion != "" {
		if err := writeJobResult(resultPath, result); err != nil {
			runErr = errors.Join(runErr, err)
			result.Conclusion = terminalErrorConclusion(ctx)
		}
	}
	if publish {
		publication, err := publishTerminalResult(agent, artifactRoot, job, planDigest, producer, result)
		if publication.MetadataMirrorError != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: warning: result metadata mirror: %v\n", publication.MetadataMirrorError)
		}
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("publish terminal result: %w", err))
		}
	}
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: run-job: %v\n", runErr)
		return 1
	}
	return 0
}

func hasGitHubActionLocks(locks []plan.ActionLock) bool {
	for _, lock := range locks {
		if lock.Source == "github" {
			return true
		}
	}
	return false
}

func resultProducer(job plan.Job, planDigest string) (transport.Producer, bool, error) {
	if os.Getenv("BUILDKITE") == "" {
		if len(job.NeedSources) != 0 {
			return transport.Producer{}, false, fmt.Errorf("plans with prerequisites require Buildkite result identity")
		}
		return transport.Producer{}, false, nil
	}
	expectedDigest := os.Getenv("BUILDKITE_GHA_PLAN_DIGEST")
	if expectedDigest == "" {
		return transport.Producer{}, false, fmt.Errorf("result publication in Buildkite requires BUILDKITE_GHA_PLAN_DIGEST")
	}
	if expectedDigest != planDigest {
		return transport.Producer{}, false, fmt.Errorf("plan digest %q does not match expected digest %q", planDigest, expectedDigest)
	}
	producer := transport.Producer{
		BuildID: os.Getenv("BUILDKITE_BUILD_ID"),
		JobID:   os.Getenv("BUILDKITE_JOB_ID"),
		StepKey: os.Getenv("BUILDKITE_STEP_KEY"),
	}
	if err := producer.Validate(); err != nil {
		return transport.Producer{}, false, fmt.Errorf("result publication in Buildkite requires valid BUILDKITE_BUILD_ID, BUILDKITE_JOB_ID, and BUILDKITE_STEP_KEY: %w", err)
	}
	if producer.StepKey != job.Target.StepKey {
		return transport.Producer{}, false, fmt.Errorf("result producer step %q does not match plan target %q", producer.StepKey, job.Target.StepKey)
	}
	return producer, true, nil
}

func writeJobResult(path string, result gharuntime.JobResult) error {
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func terminalErrorConclusion(ctx context.Context) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	return "failure"
}

func publishTerminalResult(agent transport.Agent, root string, job plan.Job, planDigest string, producer transport.Producer, result gharuntime.JobResult) (transport.Publication, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resultPublicationTimeout)
	defer cancel()
	workflow := strings.TrimPrefix(job.Workflow.Digest, "sha256:")
	return gharuntime.PublishJobResult(ctx, agent, root, workflow, job.Target.StepKey, planDigest, producer, result)
}

func runJobArgs(args []string) (planPath, resultPath string, err error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--plan", "--result":
			option := args[i]
			if seen[option] {
				return "", "", fmt.Errorf("%s may only be specified once", option)
			}
			seen[option] = true
			i++
			if i == len(args) {
				return "", "", fmt.Errorf("%s requires a path", option)
			}
			if option == "--plan" {
				planPath = args[i]
			} else {
				resultPath = args[i]
			}
		default:
			return "", "", fmt.Errorf("unknown option %q", args[i])
		}
	}
	if planPath == "" {
		return "", "", fmt.Errorf("--plan is required")
	}
	return planPath, resultPath, nil
}

func verifyBuildkiteTarget(job plan.Job) error {
	stepKey := os.Getenv("BUILDKITE_STEP_KEY")
	queue := os.Getenv("BUILDKITE_AGENT_META_DATA_QUEUE")
	if os.Getenv("BUILDKITE") != "" && (stepKey == "" || queue == "") {
		return fmt.Errorf("buildkite execution requires BUILDKITE_STEP_KEY and BUILDKITE_AGENT_META_DATA_QUEUE")
	}
	if stepKey != "" && stepKey != job.Target.StepKey {
		return fmt.Errorf("plan targets step %q, executing step is %q", job.Target.StepKey, stepKey)
	}
	if queue != "" && queue != job.Target.Queue {
		return fmt.Errorf("plan targets queue %q, executing queue is %q", job.Target.Queue, queue)
	}
	return nil
}

func validate(args []string, stdout, stderr io.Writer, version string) int {
	workflowPath, eventPath, format, profile, err := validateArgs(args)
	if err != nil {
		return usageError(stderr, "validate: %v", err)
	}
	if profile != "" && eventPath == "" {
		return usageError(stderr, "validate: --event-path is required with --profile")
	}
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
		return 1
	}
	var event []byte
	if eventPath != "" {
		event, err = os.ReadFile(eventPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: %v\n", err)
			return 1
		}
	}

	var report compiler.Report
	if eventPath == "" {
		report, err = compiler.Validate(workflowPath, source)
	} else {
		report, err = compiler.ValidateEvent(workflowPath, source, event)
	}
	if err != nil {
		if profile == "" {
			if writeErr := compatibility.Write(stdout, format, compatibility.Blocked(workflowPath, err)); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write report: %v\n", writeErr)
			}
		} else if writeErr := compatibility.WriteProfile(stdout, format, compatibility.ProfileCompileBlocked(workflowPath, profile, err)); writeErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
		}
		return 1
	}
	if profile != "" {
		_, _, distributionDigest, executableErr := executable()
		if executableErr != nil {
			if writeErr := compatibility.WriteProfile(stdout, format, compatibility.ProfileNotEvaluated(workflowPath, profile, report.LogicalJobs, report.Instances, "E_ENVIRONMENT", executableErr)); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
			}
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		preflight, profileErr := compileHostedTokenless(ctx, workflowPath, source, event, version, distributionDigest, "buildkite-gha-profile-importer")
		if profileErr != nil {
			if ctx.Err() != nil || errors.Is(profileErr, context.Canceled) {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: profile evaluation interrupted: %v\n", profileErr)
				return 1
			}
			var failure *hostedTokenlessFailure
			if errors.As(profileErr, &failure) && failure.Kind == hostedTokenlessAdmissionFailure {
				if writeErr := compatibility.WriteProfile(stdout, format, compatibility.ProfileBlocked(workflowPath, profile, report.LogicalJobs, report.Instances, profileErr)); writeErr != nil {
					_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
				}
				return 1
			}
			code := "E_PROFILE_EVALUATION"
			if errors.As(profileErr, &failure) && failure.Kind == hostedTokenlessEnvironmentFailure {
				code = "E_ENVIRONMENT"
			}
			if writeErr := compatibility.WriteProfile(stdout, format, compatibility.ProfileNotEvaluated(workflowPath, profile, report.LogicalJobs, report.Instances, code, profileErr)); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
			}
			return 1
		}
		if writeErr := compatibility.WriteProfile(stdout, format, compatibility.Admitted(workflowPath, profile, report.LogicalJobs, report.Instances, preflight.HasActions)); writeErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write profile report: %v\n", writeErr)
			return 1
		}
		return 0
	}
	if err := compatibility.Write(stdout, format, compatibility.Compilable(workflowPath, report.LogicalJobs, report.Instances)); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: validate: write report: %v\n", err)
		return 1
	}
	return 0
}

func validateArgs(args []string) (workflowPath, eventPath, format, profile string, err error) {
	format = "text"
	filtered := make([]string, 0, len(args))
	formatSeen := false
	profileSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" && args[i] != "--profile" {
			filtered = append(filtered, args[i])
			continue
		}
		option := args[i]
		if option == "--format" && formatSeen {
			return "", "", "", "", fmt.Errorf("--format may only be specified once")
		}
		if option == "--profile" && profileSeen {
			return "", "", "", "", fmt.Errorf("--profile may only be specified once")
		}
		i++
		if i == len(args) {
			return "", "", "", "", fmt.Errorf("%s requires a value", option)
		}
		if option == "--format" {
			formatSeen = true
			format = args[i]
			if format != "text" && format != "json" {
				return "", "", "", "", fmt.Errorf("--format must be text or json")
			}
		} else {
			profileSeen = true
			profile = args[i]
			if profile != hostedTokenlessProfile {
				return "", "", "", "", fmt.Errorf("--profile must be %q", hostedTokenlessProfile)
			}
		}
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	return workflowPath, eventPath, format, profile, err
}

func compile(args []string, stdout, stderr io.Writer, version string) int {
	workflowPath, eventPath, format, err := compileArgs(args)
	if err != nil {
		return usageError(stderr, "compile: %v", err)
	}
	if eventPath == "" {
		return usageError(stderr, "compile: --event-path is required")
	}
	source, err := os.ReadFile(workflowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	event, err := os.ReadFile(eventPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	var result []byte
	if format == "ir-json" {
		result, err = compiler.Compile(workflowPath, source, event)
	} else {
		digest, digestErr := executableDigest()
		if digestErr != nil {
			_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", digestErr)
			return 1
		}
		bundle, compileErr := compiler.CompileBundle(workflowPath, source, event, version, digest, "gha-importer")
		err = compileErr
		result = bundle.Pipeline
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(result); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: compile: write output: %v\n", err)
		return 1
	}
	return 0
}

func upload(args []string, stdout, stderr io.Writer, version string, agent transport.Agent) int {
	workflowPath, eventPath, _, err := uploadArgs(args)
	if err != nil {
		return usageError(stderr, "upload: %v", err)
	}
	importerStep := os.Getenv("BUILDKITE_STEP_KEY")
	if os.Getenv("BUILDKITE") != "true" || strings.TrimSpace(importerStep) == "" {
		return usageError(stderr, "upload: BUILDKITE=true and BUILDKITE_STEP_KEY are required")
	}
	workflowSource, err := os.ReadFile(workflowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	var eventSource []byte
	if eventPath != "" {
		eventSource, err = os.ReadFile(eventPath)
	} else {
		eventSource, err = buildkiteEventSource(os.Getenv)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	executablePath, executableContents, distributionDigest, err := executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	preflight, err := compileHostedTokenless(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	bundle := preflight.Bundle
	artifacts := make([]transport.Artifact, 0, 1+len(bundle.Plans))
	distributionPath, err := buildkitepipeline.DistributionPath(distributionDigest)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	artifacts = append(artifacts, transport.Artifact{Path: distributionPath, Digest: distributionDigest, Contents: executableContents})
	for _, jobPlan := range bundle.Plans {
		artifacts = append(artifacts, transport.Artifact{Path: jobPlan.Path, Digest: jobPlan.Digest, Contents: jobPlan.Contents})
	}
	root, err := os.MkdirTemp("", "buildkite-gha-upload-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: create artifact root: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	if err := transport.UploadArtifacts(ctx, agent, root, artifacts, bundle.Pipeline); err != nil {
		_, _ = fmt.Fprintf(stderr, "buildkite-gha: upload: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Uploaded %d jobs from %s with importer %s.\n", len(bundle.Plans), executablePath, importerStep)
	return 0
}

type hostedTokenlessCompilation struct {
	Bundle     compiler.Bundle
	HasActions bool
}

type hostedTokenlessFailureKind string

const (
	hostedTokenlessEnvironmentFailure hostedTokenlessFailureKind = "environment"
	hostedTokenlessEvaluationFailure  hostedTokenlessFailureKind = "evaluation"
	hostedTokenlessAdmissionFailure   hostedTokenlessFailureKind = "admission"
)

type hostedTokenlessFailure struct {
	Kind hostedTokenlessFailureKind
	Err  error
}

func (e *hostedTokenlessFailure) Error() string { return e.Err.Error() }
func (e *hostedTokenlessFailure) Unwrap() error { return e.Err }

func hostedTokenlessError(kind hostedTokenlessFailureKind, err error) error {
	return &hostedTokenlessFailure{Kind: kind, Err: err}
}

func compileHostedTokenless(ctx context.Context, workflowPath string, workflowSource, eventSource []byte, version, distributionDigest, importerStep string) (hostedTokenlessCompilation, error) {
	preflight, err := compiler.Compile(workflowPath, workflowSource, eventSource)
	if err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, err)
	}
	var ir compiler.IR
	if err := json.Unmarshal(preflight, &ir); err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, fmt.Errorf("decode compiler preflight: %w", err))
	}
	hasActions := irUsesActions(ir)
	options := compiler.Options{
		EventTrust: compiler.EventUntrusted,
		Runners: compiler.RunnerPolicy{
			Labels: map[string]string{
				"ubuntu-latest": unprivilegedRuntimeQueue,
				"ubuntu-24.04":  unprivilegedRuntimeQueue,
				"ubuntu-22.04":  unprivilegedRuntimeQueue,
			},
			UntrustedQueues: []string{unprivilegedRuntimeQueue},
		},
	}
	if hasActions {
		actionRoot, err := os.MkdirTemp("", "buildkite-gha-action-source-")
		if err != nil {
			return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEnvironmentFailure, fmt.Errorf("create action source store: %w", err))
		}
		defer func() { _ = os.RemoveAll(actionRoot) }()
		resolver, err := actionsource.NewResolver(nil)
		if err != nil {
			return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEnvironmentFailure, fmt.Errorf("configure public action resolver: %w", err))
		}
		store, err := actionsource.NewStore(actionRoot, nil)
		if err != nil {
			return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEnvironmentFailure, fmt.Errorf("configure public action source store: %w", err))
		}
		options.ResolveActions = true
		options.ActionSource = compiler.PublicActionSource{Resolver: resolver, Store: store}
	}
	bundle, err := compiler.CompileBundleContext(ctx, workflowPath, workflowSource, eventSource, version, distributionDigest, importerStep, options)
	if err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, err)
	}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessAdmissionFailure, err)
	}
	if !hasActions && bundleUsesActions(bundle) {
		return hostedTokenlessCompilation{}, hostedTokenlessError(hostedTokenlessEvaluationFailure, fmt.Errorf("final compilation introduced actions absent from preflight"))
	}
	return hostedTokenlessCompilation{Bundle: bundle, HasActions: hasActions}, nil
}

func validateUnprivilegedBundle(bundle compiler.Bundle) error {
	for _, artifact := range bundle.Plans {
		for _, capability := range artifact.Job.RequiredCapabilities {
			if capability == "docker" && !slices.Equal(artifact.Authorization.DockerCapabilitySources, []string{"dockerfile-actions"}) {
				if slices.Contains(artifact.Authorization.DockerCapabilitySources, "job-containers") || slices.Contains(artifact.Authorization.DockerCapabilitySources, "service-containers") {
					return fmt.Errorf("job %q uses job or service containers, which hosted-tokenless upload does not admit", artifact.Job.Workflow.LogicalJobID)
				}
				return fmt.Errorf("job %q requires docker without compiler-verified Dockerfile action provenance", artifact.Job.Workflow.LogicalJobID)
			}
			if capability != "network" && capability != "docker" {
				return fmt.Errorf("job %q requires capability %q, unavailable to unprivileged upload", artifact.Job.Workflow.LogicalJobID, capability)
			}
		}
		for _, action := range artifact.Job.Actions {
			if action.Source != "github" {
				continue
			}
			var service string
			switch strings.ToLower(action.Repository) {
			case "actions/upload-artifact", "actions/download-artifact":
				service = "artifact"
			case "actions/cache":
				service = "cache"
			}
			if service != "" {
				return fmt.Errorf("job %q uses action %q, which requires the unavailable GitHub Actions %s service; Phase 6 is required", artifact.Job.Workflow.LogicalJobID, action.Repository, service)
			}
		}
	}
	return nil
}

func irUsesActions(ir compiler.IR) bool {
	for _, job := range ir.Jobs {
		for _, step := range job.Steps {
			if step.Uses != "" {
				return true
			}
		}
	}
	return false
}

func bundleUsesActions(bundle compiler.Bundle) bool {
	for _, artifact := range bundle.Plans {
		if len(artifact.Job.Actions) != 0 {
			return true
		}
		for _, step := range artifact.Job.Steps {
			if step.Uses != "" || step.Action != nil || step.Kind == "uses" {
				return true
			}
		}
	}
	return false
}

func uploadArgs(args []string) (workflowPath, eventPath, runtimeQueue string, err error) {
	filtered := make([]string, 0, len(args))
	runtimeQueueSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--runtime-queue" {
			filtered = append(filtered, args[i])
			continue
		}
		if runtimeQueueSeen {
			return "", "", "", fmt.Errorf("--runtime-queue may only be specified once")
		}
		runtimeQueueSeen = true
		i++
		if i == len(args) {
			return "", "", "", fmt.Errorf("--runtime-queue requires a queue")
		}
		runtimeQueue = args[i]
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	if err != nil {
		return "", "", "", err
	}
	if !runtimeQueueSeen {
		return "", "", "", fmt.Errorf("--runtime-queue %s is required for unprivileged upload", unprivilegedRuntimeQueue)
	}
	if runtimeQueue != unprivilegedRuntimeQueue {
		return "", "", "", fmt.Errorf("--runtime-queue must be %q for unprivileged upload", unprivilegedRuntimeQueue)
	}
	return workflowPath, eventPath, runtimeQueue, err
}

func compileArgs(args []string) (workflowPath, eventPath, format string, err error) {
	format = "pipeline"
	filtered := make([]string, 0, len(args))
	formatSeen := false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" {
			filtered = append(filtered, args[i])
			continue
		}
		if formatSeen {
			return "", "", "", fmt.Errorf("--format may only be specified once")
		}
		formatSeen = true
		i++
		if i == len(args) {
			return "", "", "", fmt.Errorf("--format requires pipeline or ir-json")
		}
		format = args[i]
		if format != "pipeline" && format != "ir-json" {
			return "", "", "", fmt.Errorf("--format must be pipeline or ir-json")
		}
	}
	workflowPath, eventPath, err = workflowArgs(filtered)
	return workflowPath, eventPath, format, err
}

func executableDigest() (string, error) {
	_, _, digest, err := executable()
	return digest, err
}

func executable() (path string, contents []byte, digest string, err error) {
	path, err = os.Executable()
	if err != nil {
		return "", nil, "", fmt.Errorf("locate compiler executable: %w", err)
	}
	contents, err = os.ReadFile(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("read compiler executable: %w", err)
	}
	sum := sha256.Sum256(contents)
	return path, contents, fmt.Sprintf("sha256:%x", sum), nil
}

func workflowArgs(args []string) (workflowPath, eventPath string, err error) {
	eventPathSeen := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event-path":
			if eventPathSeen {
				return "", "", fmt.Errorf("--event-path may only be specified once")
			}
			eventPathSeen = true
			i++
			if i == len(args) {
				return "", "", fmt.Errorf("--event-path requires a path")
			}
			eventPath = args[i]
		case "-h", "--help":
			return "", "", fmt.Errorf("help must be requested immediately after the command")
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown option %q", args[i])
			}
			if workflowPath != "" {
				return "", "", fmt.Errorf("expected one workflow path")
			}
			workflowPath = args[i]
		}
	}
	if workflowPath == "" {
		return "", "", fmt.Errorf("workflow path is required")
	}
	return workflowPath, eventPath, nil
}

func usageError(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "buildkite-gha: "+format+"\n\n", args...)
	_, _ = fmt.Fprint(stderr, usage)
	return 2
}
