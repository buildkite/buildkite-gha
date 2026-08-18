package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// PlanArtifact is one immutable encoded job plan and its content-addressed path.
type PlanArtifact struct {
	Job           plan.Job
	Digest        string
	Path          string
	Contents      []byte
	Authorization PlanAuthorization
}

// PlanAuthorization is same-process compiler evidence for upload admission.
// It is deliberately not part of the encoded plan: runtimes independently
// enforce capabilities, while upload policy relies only on fresh compilation.
type PlanAuthorization struct {
	DockerCapabilitySources             []string
	ProviderTokenReadCapabilitySources  []string
	ProviderTokenWriteCapabilitySources []string
	WorkflowTokenPolicyFilename         string
	GitHubTokenActions                  []string
	GitHubTokenSecretReference          bool
}

// Bundle is the complete deterministic output of static compilation.
type Bundle struct {
	IR                IR
	Plans             []PlanArtifact
	Pipeline          []byte
	GeneratedWorkflow buildkitepipeline.Workflow
	Processing        ProcessingEvidence
}

// CompileBundle compiles an unattested event snapshot with the fail-closed
// default runner policy.
func CompileBundle(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, compilerStep string) (Bundle, error) {
	return CompileBundleWithOptions(path, source, eventSource, compilerVersion, compilerDistributionDigest, compilerStep, defaultOptions())
}

// CompileBundleWithOptions produces versioned plans and the Buildkite pipeline
// that schedules their exact static dependency graph.
func CompileBundleWithOptions(path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, compilerStep string, options Options) (Bundle, error) {
	return CompileBundleContext(context.Background(), path, source, eventSource, compilerVersion, compilerDistributionDigest, compilerStep, options)
}

// CompileBundleContext produces a complete bundle and permits cancellation
// while compilation resolves immutable public action source.
func CompileBundleContext(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest, compilerStep string, options Options) (Bundle, error) {
	bundle, err := CompileBundlePlansContext(ctx, path, source, eventSource, compilerVersion, compilerDistributionDigest, options)
	if err != nil {
		return bundle, err
	}
	return GenerateBundlePipeline(bundle, compilerDistributionDigest, compilerStep, options)
}

// CompileBundlePlansContext constructs every immutable plan but deliberately
// stops before pipeline generation so callers can apply admission policy.
func CompileBundlePlansContext(ctx context.Context, path string, source, eventSource []byte, compilerVersion, compilerDistributionDigest string, options Options) (Bundle, error) {
	if compilerVersion == "" {
		return Bundle{}, fmt.Errorf("compiler version is required")
	}
	if !strings.HasPrefix(compilerDistributionDigest, "sha256:") {
		return Bundle{}, fmt.Errorf("compiler distribution digest is required")
	}
	ir, err := compile(path, source, eventSource, options)
	if err != nil {
		return Bundle{IR: ir}, err
	}
	options.ActionSource = newMemoizedActionSource(options.ActionSource)
	evidence, err := validateActionResolutions(ctx, ir, options)
	bundle := Bundle{IR: ir, Processing: evidence}
	if err != nil {
		return bundle, err
	}
	plans, authorizations, planEvaluations, err := compilePlansWithAuthorization(ctx, ir, compilerVersion, compilerDistributionDigest, options)
	bundle.Processing.Plans = planEvaluations
	if err != nil {
		return bundle, err
	}
	if len(plans) != len(ir.Jobs) || len(authorizations) != len(plans) {
		return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("compiler produced %d plans and %d authorizations for %d job instances", len(plans), len(authorizations), len(ir.Jobs)))
	}
	warnedReusablePermissions := false
	warnedJobPermissions := false
	for i, job := range plans {
		if job.GitHubToken == nil {
			continue
		}
		if ir.Jobs[i].tokenPolicyNarrowed && !warnedReusablePermissions {
			bundle.IR.Warnings = append(bundle.IR.Warnings, reusableWorkflowTokenWarning(ir.Jobs[i].reusableCall))
			warnedReusablePermissions = true
		}
		if (ir.Jobs[i].jobPermissionsIgnored || jobPermissionsIgnored(job.GitHubToken.Permissions, ir.Jobs[i].Permissions)) && !warnedJobPermissions {
			position := ir.Jobs[i].Source.Start
			if ir.Jobs[i].jobPermissionsIgnored && ir.Jobs[i].reusableCall.Line != 0 {
				position = ir.Jobs[i].reusableCall
			}
			bundle.IR.Warnings = append(bundle.IR.Warnings, jobWorkflowTokenWarning(position))
			warnedJobPermissions = true
		}
	}

	artifacts := make([]PlanArtifact, len(plans))
	for i, job := range plans {
		if job.Target.StepKey != ir.Jobs[i].Key || job.Target.Queue != ir.Jobs[i].Queue {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("plan %d target %q/%q does not match job instance %q/%q", i, job.Target.StepKey, job.Target.Queue, ir.Jobs[i].Key, ir.Jobs[i].Queue))
		}
		expectedRuntimeDigest := options.RuntimeDistributions[ir.Jobs[i].Platform]
		if expectedRuntimeDigest == "" && ir.Jobs[i].Platform == PlatformLinuxAMD64 {
			expectedRuntimeDigest = compilerDistributionDigest
		}
		if job.Runtime == nil || job.Runtime.DistributionDigest != expectedRuntimeDigest {
			return bundle, processingFinding(StagePlans, CodePlanConstruction, "compatibility", fmt.Errorf("plan %d runtime distribution does not match job platform %s", i, ir.Jobs[i].Platform))
		}
		contents, err := plan.Encode(job)
		if err != nil {
			return bundle, &ProcessingFinding{Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility", Job: job.Workflow.LogicalJobID, Instance: ir.Jobs[i].Key, Err: fmt.Errorf("encode plan for job %q: %w", job.Workflow.LogicalJobID, err)}
		}
		digest := transport.Digest(contents)
		planPath, err := buildkitepipeline.PlanPath(digest)
		if err != nil {
			return bundle, &ProcessingFinding{Stage: StagePlans, Code: CodePlanConstruction, Category: "compatibility", Job: job.Workflow.LogicalJobID, Instance: ir.Jobs[i].Key, Err: fmt.Errorf("locate plan for job %q: %w", job.Workflow.LogicalJobID, err)}
		}
		artifacts[i] = PlanArtifact{Job: job, Digest: digest, Path: planPath, Contents: contents, Authorization: authorizations[i]}
	}
	bundle.Plans = artifacts
	bundle.Processing.PlansConstructed = true
	return bundle, nil
}

func jobPermissionsIgnored(workflowPermissions, effectivePermissions map[string]string) bool {
	normalized := make(map[string]string, len(effectivePermissions))
	for name, access := range effectivePermissions {
		if name != "id-token" {
			normalized[strings.ReplaceAll(name, "-", "_")] = access
		}
	}
	return !maps.Equal(workflowPermissions, normalized)
}

// GenerateBundlePipeline emits pipeline bytes only after plan construction and
// any caller-owned admission stage have succeeded.
func GenerateBundlePipeline(bundle Bundle, compilerDistributionDigest, compilerStep string, options Options) (Bundle, error) {
	ir, artifacts := bundle.IR, bundle.Plans
	if len(artifacts) != len(ir.Jobs) {
		return bundle, processingFinding(StagePipeline, CodePipelineGeneration, "compatibility", fmt.Errorf("compiler has %d plans for %d job instances", len(artifacts), len(ir.Jobs)))
	}
	jobs := make([]buildkitepipeline.Job, len(artifacts))
	for i, artifact := range artifacts {
		job := artifact.Job
		jobs[i] = buildkitepipeline.Job{
			Key:                ir.Jobs[i].Key,
			Label:              ir.Jobs[i].Label,
			CheckLabel:         instanceCheckLabel(ir.Jobs[i]),
			Queue:              ir.Jobs[i].Queue,
			Platform:           ir.Jobs[i].Platform.String(),
			DistributionDigest: job.RuntimeDistributionDigest(),
			PlanDigest:         artifact.Digest,
			Dependencies:       append([]string(nil), ir.Jobs[i].Needs...),
			RequiresMise:       job.NeedsMise(),
			SoftFail:           job.ContinueOnError,
		}
		if ir.Jobs[i].Platform == PlatformLinuxAMD64 {
			jobs[i].RuntimeImage = ir.Jobs[i].RuntimeImage
			if jobs[i].RuntimeImage == "" {
				jobs[i].RuntimeImage = options.RuntimeImage
			}
		}
		if ir.Jobs[i].ConcurrencyGroup != "" {
			jobs[i].Concurrency = 1
			jobs[i].ConcurrencyGroup = buildkiteConcurrencyGroup(ir.Event.Repository, ir.Jobs[i].ConcurrencyGroup)
		} else if ir.Jobs[i].MaxParallel != nil {
			jobs[i].Concurrency = *ir.Jobs[i].MaxParallel
			workflowScope := strings.TrimPrefix(ir.Workflow.Digest, "sha256:")
			if options.StepKeyNamespace != "" {
				workflowScope += "/" + options.StepKeyNamespace
			}
			jobs[i].ConcurrencyGroup = "buildkite-gha/" + workflowScope + "/" + ir.Jobs[i].LogicalJobID
		}
	}
	var concurrencyGate *buildkitepipeline.ConcurrencyGate
	if ir.Workflow.ConcurrencyGroup != "" {
		concurrencyGate = &buildkitepipeline.ConcurrencyGate{
			Group: buildkiteConcurrencyGroup(ir.Event.Repository, ir.Workflow.ConcurrencyGroup),
			Queue: jobs[0].Queue,
		}
	}
	generatedWorkflow := buildkitepipeline.Workflow{
		ConcurrencyGate: concurrencyGate,
		Jobs:            jobs,
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		CompilerStep:    compilerStep,
		GroupLabel:      options.GroupLabel,
		ConcurrencyGate: generatedWorkflow.ConcurrencyGate,
		Jobs:            generatedWorkflow.Jobs,
	})
	if err != nil {
		return bundle, processingFinding(StagePipeline, CodePipelineGeneration, "compatibility", fmt.Errorf("emit Buildkite pipeline: %w", err))
	}
	bundle.Pipeline = pipeline
	bundle.GeneratedWorkflow = generatedWorkflow
	bundle.Processing.PipelineGenerated = true
	return bundle, nil
}

func buildkiteConcurrencyGroup(repository Repository, group string) string {
	scope := strings.ToLower(repository.Owner+"/"+repository.Name) + "\x00" + canonicalConcurrencyGroup(group)
	digest := sha256.Sum256([]byte(scope))
	return "buildkite-gha/concurrency/" + hex.EncodeToString(digest[:])
}
