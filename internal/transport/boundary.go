package transport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
)

// Runner is the only process boundary used by the Buildkite adapter.
type Runner interface {
	Run(ctx context.Context, dir, name string, args []string, stdin []byte) ([]byte, error)
}

// Agent invokes public buildkite-agent commands. Tests use a capture Runner.
type Agent struct {
	Runner Runner
}

func (a Agent) UploadArtifact(ctx context.Context, path string) error {
	_, err := a.run(ctx, []string{"artifact", "upload", path}, nil)
	return err
}

func (a Agent) uploadArtifactFrom(ctx context.Context, root, path string) error {
	_, err := a.runInDir(ctx, root, []string{"artifact", "upload", path}, nil)
	return err
}

func (a Agent) DownloadArtifact(ctx context.Context, path, destination, producerStep string) error {
	if !keyPattern.MatchString(producerStep) {
		return fmt.Errorf("invalid producer step key %q", producerStep)
	}
	_, err := a.run(ctx, []string{"artifact", "download", path, destination, "--step", producerStep}, nil)
	return err
}

func (a Agent) SetMetadata(ctx context.Context, key, value string) error {
	_, err := a.run(ctx, []string{"meta-data", "set", key, value}, nil)
	return err
}

func (a Agent) GetMetadata(ctx context.Context, key string) ([]byte, error) {
	return a.run(ctx, []string{"meta-data", "get", key}, nil)
}

func (a Agent) UploadPipeline(ctx context.Context, pipeline []byte) error {
	_, err := a.run(ctx, []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets", "--reject-parse-warnings"}, pipeline)
	return err
}

func (a Agent) run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	return a.runInDir(ctx, "", args, stdin)
}

func (a Agent) runInDir(ctx context.Context, dir string, args []string, stdin []byte) ([]byte, error) {
	if a.Runner == nil {
		return nil, fmt.Errorf("buildkite agent runner is required")
	}
	return a.Runner.Run(ctx, dir, "buildkite-agent", args, stdin)
}

// Upload materializes immutable plan artifacts below a caller-owned root, then
// executes the security-relevant ordering: artifacts, signed expected marker,
// dynamic pipeline, and signed completed marker.
func Upload(ctx context.Context, agent Agent, root string, plans []PlanArtifact, pipeline []byte, expected, completed MarkerValue) error {
	plans = clonePlans(plans)
	if err := validateMarkerPair(pipeline, plans, expected, completed); err != nil {
		return err
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].StepKey < plans[j].StepKey })
	materialized, err := materializePlans(root, plans)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		files := materialized[plan.StepKey]
		if err := verifyMaterialized(files.plan, plan.Contents, plan.Digest); err != nil {
			return fmt.Errorf("verify plan %q before upload: %w", plan.StepKey, err)
		}
		if err := agent.uploadArtifactFrom(ctx, files.root, plan.Path()); err != nil {
			return fmt.Errorf("upload plan %q: %w", plan.StepKey, err)
		}
		if err := verifyMaterialized(files.binding, plan.Binding, ""); err != nil {
			return fmt.Errorf("verify plan binding %q before upload: %w", plan.StepKey, err)
		}
		if err := agent.uploadArtifactFrom(ctx, files.root, plan.BindingPath()); err != nil {
			return fmt.Errorf("upload plan binding %q: %w", plan.StepKey, err)
		}
	}
	if err := agent.SetMetadata(ctx, expected.key, expected.value); err != nil {
		return fmt.Errorf("publish expected marker: %w", err)
	}
	if err := agent.UploadPipeline(ctx, pipeline); err != nil {
		return fmt.Errorf("upload pipeline: %w", err)
	}
	if err := agent.SetMetadata(ctx, completed.key, completed.value); err != nil {
		return fmt.Errorf("publish completed marker: %w", err)
	}
	return nil
}

type materializedPlan struct {
	root    string
	plan    string
	binding string
}

func clonePlans(plans []PlanArtifact) []PlanArtifact {
	cloned := make([]PlanArtifact, len(plans))
	for i, plan := range plans {
		cloned[i] = plan
		cloned[i].Contents = bytes.Clone(plan.Contents)
		cloned[i].Binding = bytes.Clone(plan.Binding)
	}
	return cloned
}

func materializePlans(root string, plans []PlanArtifact) (map[string]materializedPlan, error) {
	if root == "" {
		return nil, fmt.Errorf("plan artifact root is required")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve plan artifact root: %w", err)
	}
	rootFS, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("open plan artifact root: %w", err)
	}
	defer func() { _ = rootFS.Close() }()

	materialized := make(map[string]materializedPlan, len(plans))
	for _, plan := range plans {
		planPath, err := writeMaterialized(rootFS, absoluteRoot, plan.Path(), plan.Contents)
		if err != nil {
			return nil, fmt.Errorf("materialize plan %q: %w", plan.StepKey, err)
		}
		bindingPath, err := writeMaterialized(rootFS, absoluteRoot, plan.BindingPath(), plan.Binding)
		if err != nil {
			return nil, fmt.Errorf("materialize plan binding %q: %w", plan.StepKey, err)
		}
		rootResolved, err := filepath.EvalSymlinks(absoluteRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve plan artifact root: %w", err)
		}
		materialized[plan.StepKey] = materializedPlan{root: rootResolved, plan: planPath, binding: bindingPath}
	}
	return materialized, nil
}

func writeMaterialized(root *os.Root, absoluteRoot, relative string, contents []byte) (string, error) {
	relative = filepath.FromSlash(relative)
	if err := root.MkdirAll(filepath.Dir(relative), 0o700); err != nil {
		return "", err
	}
	if err := root.Remove(relative); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := root.WriteFile(relative, contents, 0o600); err != nil {
		return "", err
	}
	unresolved := filepath.Join(absoluteRoot, relative)
	resolved, err := filepath.EvalSymlinks(unresolved)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(rootResolved, resolved)
	if err != nil || within == ".." || filepath.IsAbs(within) || len(within) >= 3 && within[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("materialized path escaped artifact root")
	}
	return resolved, nil
}

func verifyMaterialized(path string, expected []byte, digest string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(contents, expected) {
		return fmt.Errorf("on-disk bytes differ from validated contents")
	}
	if digest == "" {
		digest = Digest(expected)
	}
	if Digest(contents) != digest {
		return fmt.Errorf("on-disk digest differs from validated digest")
	}
	return nil
}

func validateMarkerPair(pipeline []byte, plans []PlanArtifact, expected, completed MarkerValue) error {
	digest := Digest(pipeline)
	planJobs := make([]UploadJob, 0, len(plans))
	for _, plan := range plans {
		if err := plan.validate(); err != nil {
			return err
		}
		planJobs = append(planJobs, UploadJob{Key: plan.StepKey, PlanDigest: plan.Digest})
	}
	sort.Slice(planJobs, func(i, j int) bool { return planJobs[i].Key < planJobs[j].Key })
	if expected.phase != "expected" || completed.phase != "completed" ||
		expected.importerKey == "" || expected.importerKey != completed.importerKey ||
		expected.pipelineDigest != digest || completed.pipelineDigest != digest ||
		expected.key != fmt.Sprintf("buildkite-gha/v1/uploads/%s/expected", expected.importerKey) ||
		completed.key != fmt.Sprintf("buildkite-gha/v1/uploads/%s/completed", expected.importerKey) ||
		!slices.Equal(expected.jobs, completed.jobs) || !slices.Equal(expected.jobs, planJobs) ||
		expected.value == "" || completed.value == "" {
		return fmt.Errorf("signed upload markers do not match pipeline and importer")
	}
	return nil
}
