package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
}

// Bundle is the complete deterministic output of static compilation.
type Bundle struct {
	IR       IR
	Plans    []PlanArtifact
	Pipeline []byte
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
	if compilerVersion == "" {
		return Bundle{}, fmt.Errorf("compiler version is required")
	}
	if !strings.HasPrefix(compilerDistributionDigest, "sha256:") {
		return Bundle{}, fmt.Errorf("compiler distribution digest is required")
	}
	ir, err := compile(path, source, eventSource, options)
	if err != nil {
		return Bundle{}, err
	}
	plans, authorizations, err := compilePlansWithAuthorization(ctx, ir, compilerVersion, compilerDistributionDigest, options)
	if err != nil {
		return Bundle{}, err
	}
	if len(plans) != len(ir.Jobs) || len(authorizations) != len(plans) {
		return Bundle{}, fmt.Errorf("compiler produced %d plans and %d authorizations for %d job instances", len(plans), len(authorizations), len(ir.Jobs))
	}

	artifacts := make([]PlanArtifact, len(plans))
	jobs := make([]buildkitepipeline.Job, len(plans))
	for i, job := range plans {
		if job.Target.StepKey != ir.Jobs[i].Key || job.Target.Queue != ir.Jobs[i].Queue {
			return Bundle{}, fmt.Errorf("plan %d target %q/%q does not match job instance %q/%q", i, job.Target.StepKey, job.Target.Queue, ir.Jobs[i].Key, ir.Jobs[i].Queue)
		}
		contents, err := plan.Encode(job)
		if err != nil {
			return Bundle{}, fmt.Errorf("encode plan for job %q: %w", job.Workflow.LogicalJobID, err)
		}
		digest := transport.Digest(contents)
		planPath, err := buildkitepipeline.PlanPath(digest)
		if err != nil {
			return Bundle{}, fmt.Errorf("locate plan for job %q: %w", job.Workflow.LogicalJobID, err)
		}
		artifacts[i] = PlanArtifact{Job: job, Digest: digest, Path: planPath, Contents: contents, Authorization: authorizations[i]}
		jobs[i] = buildkitepipeline.Job{
			Key:          ir.Jobs[i].Key,
			Label:        ir.Jobs[i].Label,
			Queue:        ir.Jobs[i].Queue,
			PlanDigest:   digest,
			Dependencies: append([]string(nil), ir.Jobs[i].Needs...),
			RequiresMise: job.NeedsMise(),
		}
		if ir.Jobs[i].ConcurrencyGroup != "" {
			jobs[i].Concurrency = 1
			jobs[i].ConcurrencyGroup = buildkiteConcurrencyGroup(ir.Event.Repository, ir.Jobs[i].ConcurrencyGroup)
		} else if ir.Jobs[i].MaxParallel != nil {
			jobs[i].Concurrency = *ir.Jobs[i].MaxParallel
			jobs[i].ConcurrencyGroup = "buildkite-gha/" + strings.TrimPrefix(ir.Workflow.Digest, "sha256:") + "/" + ir.Jobs[i].LogicalJobID
		}
	}
	var concurrencyGate *buildkitepipeline.ConcurrencyGate
	if ir.Workflow.ConcurrencyGroup != "" {
		concurrencyGate = &buildkitepipeline.ConcurrencyGate{
			Group: buildkiteConcurrencyGroup(ir.Event.Repository, ir.Workflow.ConcurrencyGroup),
			Queue: jobs[0].Queue,
		}
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{
		CompilerStep:       compilerStep,
		DistributionDigest: compilerDistributionDigest,
		GroupLabel:         options.GroupLabel,
		RuntimeCacheScope:  buildkitepipeline.RuntimeCacheScope(ir.Event.Repository.Owner+"/"+ir.Event.Repository.Name, ir.Event.Ref),
		ConcurrencyGate:    concurrencyGate,
		Jobs:               jobs,
	})
	if err != nil {
		return Bundle{}, fmt.Errorf("emit Buildkite pipeline: %w", err)
	}
	return Bundle{IR: ir, Plans: artifacts, Pipeline: pipeline}, nil
}

func buildkiteConcurrencyGroup(repository Repository, group string) string {
	scope := strings.ToLower(repository.Owner+"/"+repository.Name) + "\x00" + canonicalConcurrencyGroup(group)
	digest := sha256.Sum256([]byte(scope))
	return "buildkite-gha/concurrency/" + hex.EncodeToString(digest[:])
}
