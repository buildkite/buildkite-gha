package compiler

import (
	"fmt"
	"strings"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

// PlanArtifact is one immutable encoded job plan and its content-addressed path.
type PlanArtifact struct {
	Job      plan.Job
	Digest   string
	Path     string
	Contents []byte
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
	plans, err := compilePlans(ir, compilerVersion, compilerDistributionDigest)
	if err != nil {
		return Bundle{}, err
	}
	if len(plans) != len(ir.Jobs) {
		return Bundle{}, fmt.Errorf("compiler produced %d plans for %d job instances", len(plans), len(ir.Jobs))
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
		artifacts[i] = PlanArtifact{Job: job, Digest: digest, Path: planPath, Contents: contents}
		jobs[i] = buildkitepipeline.Job{
			Key:          ir.Jobs[i].Key,
			Label:        ir.Jobs[i].Label,
			Queue:        ir.Jobs[i].Queue,
			PlanDigest:   digest,
			Dependencies: append([]string(nil), ir.Jobs[i].Needs...),
		}
		if ir.Jobs[i].MaxParallel != nil {
			jobs[i].Concurrency = *ir.Jobs[i].MaxParallel
			jobs[i].ConcurrencyGroup = "buildkite-gha/" + strings.TrimPrefix(ir.Workflow.Digest, "sha256:") + "/" + ir.Jobs[i].LogicalJobID
		}
	}
	pipeline, err := buildkitepipeline.Emit(buildkitepipeline.Pipeline{CompilerStep: compilerStep, Jobs: jobs})
	if err != nil {
		return Bundle{}, fmt.Errorf("emit Buildkite pipeline: %w", err)
	}
	return Bundle{IR: ir, Plans: artifacts, Pipeline: pipeline}, nil
}
