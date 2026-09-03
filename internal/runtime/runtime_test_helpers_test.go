package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
	executionprogram "github.com/buildkite/buildkite-gha/internal/program"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

func runtimePlan(t *testing.T, workspace, workflowPath string, steps []plan.Step) plan.Job {
	t.Helper()
	source, err := os.ReadFile(filepath.Join(workspace, workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(source)
	requiresMise := true
	job := plan.Job{
		Schema: plan.Schema, Compiler: plan.Compiler{Version: "0.0.0-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Runtime:      &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow:     plan.Workflow{Path: workflowPath, Digest: "sha256:" + hex.EncodeToString(digest[:]), LogicalJobID: "fixture"},
		Event:        plan.Event{Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64)},
		Target:       plan.Target{StepKey: "gha-fixture", Queue: "ubuntu-latest"},
		Steps:        steps,
		RequiresMise: &requiresMise,
	}
	attachTestProgram(&job)
	return job
}

func attachTestProgram(job *plan.Job) {
	actions := map[string]executionprogram.Action(nil)
	if job.Program != nil {
		actions = job.Program.Actions
	}
	program := executionprogram.Program{Version: executionprogram.Version, Actions: actions, Job: executionprogram.Job{
		Condition:       testProgramSite(job.Condition, executionprogram.SurfaceJobCondition, executionprogram.ResultBoolean),
		ContinueOnError: job.ContinueOnError,
		TimeoutMinutes:  job.TimeoutMinutes,
		Env:             testProgramBindings(job.Env, executionprogram.SurfaceJobEnvironment),
		Defaults: executionprogram.Defaults{
			Shell:            testProgramSite(job.DefaultShell, executionprogram.SurfaceJobDefault, executionprogram.ResultString),
			WorkingDirectory: testProgramSite(job.DefaultWorkingDirectory, executionprogram.SurfaceJobDefault, executionprogram.ResultString),
		},
		Outputs: testProgramBindings(job.Outputs, executionprogram.SurfaceJobOutput),
	}}
	program.Job.Guards = make([]executionprogram.Guard, len(job.CallGuards))
	for i, guard := range job.CallGuards {
		program.Job.Guards[i].Condition = testProgramSite(guard.Condition, executionprogram.SurfaceCallCondition, executionprogram.ResultBoolean)
	}
	program.Job.Steps = make([]executionprogram.Step, len(job.Steps))
	for i := range job.Steps {
		step := &job.Steps[i]
		normalized := executionprogram.Step{
			ID: step.ID, Kind: step.Kind, Background: step.Background, Targets: append([]string(nil), step.Targets...),
			Env:             testProgramBindings(step.Env, executionprogram.SurfaceStepTemplate),
			Condition:       testProgramSite(step.Condition, executionprogram.SurfaceStepCondition, executionprogram.ResultBoolean),
			ContinueOnError: executionprogram.BoolControl{Literal: step.ContinueOnError},
			TimeoutMinutes:  executionprogram.NumberControl{Literal: step.TimeoutMinutes},
			Name:            testProgramSite(step.Name, executionprogram.SurfaceStepTemplate, executionprogram.ResultString),
		}
		if step.ContinueOnErrorExpression != "" {
			site := testProgramSite(step.ContinueOnErrorExpression, executionprogram.SurfaceStepControl, executionprogram.ResultBoolean)
			normalized.ContinueOnError.Expression = &site
		}
		if step.TimeoutMinutesExpression != "" {
			site := testProgramSite(step.TimeoutMinutesExpression, executionprogram.SurfaceStepControl, executionprogram.ResultNumber)
			normalized.TimeoutMinutes.Expression = &site
		}
		switch step.Kind {
		case "run":
			normalized.Run = &executionprogram.Run{
				Command:          testProgramSite(step.Command, executionprogram.SurfaceStepTemplate, executionprogram.ResultString),
				Shell:            testProgramSite(step.Shell, executionprogram.SurfaceStepTemplate, executionprogram.ResultString),
				WorkingDirectory: testProgramSite(step.WorkingDirectory, executionprogram.SurfaceStepTemplate, executionprogram.ResultString),
			}
		case "uses":
			normalized.Invocation = &executionprogram.Invocation{
				Uses: testProgramSite(step.Uses, executionprogram.SurfaceRuntimeTemplate, executionprogram.ResultString),
				With: testProgramBindingsPurpose(step.With, executionprogram.SurfaceStepTemplate, executionprogram.PurposeActionInput),
			}
			if step.Action != nil {
				normalized.Invocation.Lock = step.Action.Lock
			}
		}
		program.Job.Steps[i] = normalized
		step.Execution = &program.Job.Steps[i]
	}
	if job.Container != nil {
		program.Job.Container = &executionprogram.Container{
			Image: testProgramSite(job.Container.Image, executionprogram.SurfaceRuntimeTemplate, executionprogram.ResultString),
			Env:   testProgramBindings(job.Container.Env, executionprogram.SurfaceRuntimeTemplate),
			Ports: testProgramSites(job.Container.Ports, executionprogram.SurfaceRuntimeTemplate),
		}
	}
	if job.ServicesExpression != "" {
		site := testProgramSite(job.ServicesExpression, executionprogram.SurfaceServiceMap, executionprogram.ResultObject)
		program.Job.Services.Dynamic = &site
	} else {
		for _, name := range job.ServiceOrder {
			service := job.Services[name]
			container := executionprogram.ServiceContainer{
				Image:      testProgramSite(service.Image, executionprogram.SurfaceServiceTemplate, executionprogram.ResultString),
				Env:        testProgramBindings(service.Env, executionprogram.SurfaceServiceTemplate),
				Ports:      testProgramSites(service.Ports, executionprogram.SurfaceServiceTemplate),
				Volumes:    testProgramSites(service.Volumes, executionprogram.SurfaceServiceTemplate),
				Options:    testProgramSite(service.Options, executionprogram.SurfaceServiceTemplate, executionprogram.ResultString),
				Command:    testProgramSite(service.Command, executionprogram.SurfaceServiceTemplate, executionprogram.ResultString),
				Entrypoint: testProgramSite(service.Entrypoint, executionprogram.SurfaceServiceTemplate, executionprogram.ResultString),
			}
			if service.Credentials != nil {
				container.Credentials = &executionprogram.ContainerCredentials{
					Username: testProgramSite(service.Credentials.Username, executionprogram.SurfaceServiceCredential, executionprogram.ResultString),
					Password: testProgramSite(service.Credentials.Password, executionprogram.SurfaceServiceCredential, executionprogram.ResultString),
				}
			}
			program.Job.Services.Static = append(program.Job.Services.Static, executionprogram.Service{Name: name, Container: container})
		}
	}
	job.Program = &program
}

// runTestJob upgrades direct runtime fixtures to the normalized plan contract
// at their execution boundary. Compiler-produced plans already contain these
// programs; rebuilding their projection only incorporates deliberate fixture
// mutations made after compilation.

func (runner Runner) runTestJob(ctx context.Context, job plan.Job, workspace string) (JobResult, error) {
	if workspace != "" {
		if err := verifyWorkflow(job, workspace); err != nil && !errors.Is(err, os.ErrNotExist) {
			return JobResult{Conclusion: "failure", Outputs: map[string]string{}, Env: map[string]string{}, State: map[string]string{}, Artifacts: []transport.ResultArtifact{}}, err
		}
	}
	if err := synthesizeTestLocalActionLocks(&job, workspace); err != nil {
		return JobResult{}, err
	}
	attachTestProgram(&job)
	if err := attachTestActionPrograms(&job, workspace, runner.Actions); err != nil {
		return JobResult{}, err
	}
	return runner.RunJob(ctx, job, workspace)
}

func synthesizeTestLocalActionLocks(job *plan.Job, workspace string) error {
	byPath := make(map[string]string, len(job.Actions))
	for _, lock := range job.Actions {
		if lock.Source == "workspace" {
			byPath[lock.Path] = lock.ID
		}
	}
	var ensure func(string) (string, error)
	ensure = func(path string) (string, error) {
		path = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
		if lockID, ok := byPath[path]; ok {
			return lockID, nil
		}
		actionRoot := filepath.Join(workspace, filepath.FromSlash(path))
		digest, err := source.DigestTree(actionRoot)
		if err != nil {
			return "", err
		}
		lockID := fmt.Sprintf("a-%016x", len(job.Actions)+1)
		byPath[path] = lockID
		job.Actions = append(job.Actions, plan.ActionLock{ID: lockID, Source: "workspace", Path: path, SourceDigest: digest})
		lockIndex := len(job.Actions) - 1
		loaded, err := metadata.Load(workspace, path)
		if err != nil {
			return "", err
		}
		actionRuntime, err := loaded.Runtime()
		if err != nil {
			var unsupported *metadata.UnsupportedRuntimeError
			if errors.As(err, &unsupported) {
				return lockID, nil
			}
			return "", err
		}
		if actionRuntime != metadata.RuntimeComposite {
			return lockID, nil
		}
		for _, child := range loaded.Runs.Steps {
			if !strings.HasPrefix(child.Uses, "./") {
				continue
			}
			childID, err := ensure(child.Uses)
			if err != nil {
				return "", err
			}
			if job.Actions[lockIndex].Children == nil {
				job.Actions[lockIndex].Children = map[string]plan.ActionSelector{}
			}
			job.Actions[lockIndex].Children[child.Uses] = plan.ActionSelector{Lock: childID}
		}
		return lockID, nil
	}
	for i := range job.Steps {
		step := &job.Steps[i]
		if step.Kind != "uses" || step.Action != nil || !strings.HasPrefix(step.Uses, "./") {
			continue
		}
		lockID, err := ensure(step.Uses)
		if err != nil {
			return fmt.Errorf("normalize test action %q: %w", step.Uses, err)
		}
		step.Action = &plan.ActionSelector{Lock: lockID}
	}
	return nil
}

func attachTestActionPrograms(job *plan.Job, workspace string, materializer ActionMaterializer) error {
	if job.Program.Actions == nil {
		job.Program.Actions = map[string]executionprogram.Action{}
	}
	var remoteRoot string
	if fake, ok := materializer.(*fakeActionMaterializer); ok {
		remoteRoot = fake.result.RepositoryRoot
		if remoteRoot == "" {
			remoteRoot = fake.result.ActionRoot
		}
	}
	for _, lock := range job.Actions {
		if usesNativeAdapter(lock) {
			continue
		}
		if _, ok := job.Program.Actions[lock.ID]; ok {
			continue
		}
		root, path := workspace, lock.Path
		if lock.Source == "github" {
			root = remoteRoot
			if root == "" {
				return fmt.Errorf("normalize test action %q: materialized repository root is unavailable", lock.ID)
			}
			if filepath.Clean(root) == filepath.Clean(fakeActionRoot(materializer)) {
				path = "."
			}
		}
		if err := attachTestActionProgramFromRoot(job, lock.ID, root, path); err != nil {
			return err
		}
	}
	return nil
}

func attachTestActionProgramFromRoot(job *plan.Job, lockID, root, path string) error {
	if job.Program == nil {
		attachTestProgram(job)
	}
	if job.Program.Actions == nil {
		job.Program.Actions = map[string]executionprogram.Action{}
	}
	loaded, err := metadata.Load(root, path)
	if err != nil {
		return fmt.Errorf("normalize test action %q: %w", lockID, err)
	}
	actionRuntime, err := loaded.Runtime()
	if err != nil {
		var unsupported *metadata.UnsupportedRuntimeError
		if !errors.As(err, &unsupported) {
			return fmt.Errorf("normalize test action %q: %w", lockID, err)
		}
		actionRuntime = metadata.Runtime(loaded.Runs.Using)
	}
	var lock plan.ActionLock
	for _, candidate := range job.Actions {
		if candidate.ID == lockID {
			lock = candidate
			break
		}
	}
	children := make(map[string]string, len(lock.Children))
	for uses, selector := range lock.Children {
		children[uses] = selector.Lock
	}
	job.Program.Actions[lockID] = executionprogram.ActionFromMetadata(loaded, string(actionRuntime), children)
	return nil
}

func fakeActionRoot(materializer ActionMaterializer) string {
	if fake, ok := materializer.(*fakeActionMaterializer); ok {
		return fake.result.ActionRoot
	}
	return ""
}

func testActionLockResolver(t *testing.T, job plan.Job, workspace string, materializer ActionMaterializer) *actionLockResolver {
	t.Helper()
	if job.Program == nil {
		job.Program = &executionprogram.Program{Version: executionprogram.Version, Actions: map[string]executionprogram.Action{}, Job: executionprogram.Job{Condition: testProgramSite("", executionprogram.SurfaceJobCondition, executionprogram.ResultBoolean)}}
	}
	if job.Program.Actions == nil {
		job.Program.Actions = map[string]executionprogram.Action{}
	}
	for _, lock := range job.Actions {
		if usesNativeAdapter(lock) {
			continue
		}
		job.Program.Actions[lock.ID] = executionprogram.Action{
			Name: "test", Runtime: "node24", Main: "index.js",
			PreIf:  testProgramSite("", executionprogram.SurfaceActionLifecycle, executionprogram.ResultBoolean),
			PostIf: testProgramSite("", executionprogram.SurfaceActionLifecycle, executionprogram.ResultBoolean),
		}
	}
	return newActionLockResolver(job, workspace, materializer)
}

func testProgramSite(source string, surface executionprogram.Surface, result executionprogram.ResultType) executionprogram.Site {
	return executionprogram.Site{Source: source, Surface: surface, Result: result, Provenance: executionprogram.ProvenanceWorkflow, Purpose: executionprogram.PurposeExpression}
}

func testProgramBindings(values map[string]string, surface executionprogram.Surface) []executionprogram.Binding {
	return testProgramBindingsPurpose(values, surface, executionprogram.PurposeExpression)
}

func testProgramBindingsPurpose(values map[string]string, surface executionprogram.Surface, purpose executionprogram.Purpose) []executionprogram.Binding {
	bindings := make([]executionprogram.Binding, 0, len(values))
	for _, name := range sortedKeys(values) {
		value := testProgramSite(values[name], surface, executionprogram.ResultString)
		value.Purpose = purpose
		bindings = append(bindings, executionprogram.Binding{Name: name, Value: value})
	}
	return bindings
}

func testProgramSites(values []string, surface executionprogram.Surface) []executionprogram.Site {
	sites := make([]executionprogram.Site, len(values))
	for i, value := range values {
		sites[i] = testProgramSite(value, surface, executionprogram.ResultString)
	}
	return sites
}

func writeFixtureFile(t *testing.T, root, path, contents string) {
	t.Helper()
	path = filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeNodeExecutable(t *testing.T, path string, major int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, "#!/bin/sh\nprintf 'v%d.99.0\\n'\n", major), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	pathParts := append([]string{filepath.Dir(file), "..", "..", "testdata"}, parts...)
	path, err := filepath.Abs(filepath.Join(pathParts...))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireLinuxAMD64(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires a linux/amd64 runtime host")
	}
}

func requireNode24(t *testing.T) string {
	t.Helper()
	if node := os.Getenv("BUILDKITE_GHA_TEST_NODE24"); node != "" {
		if _, err := discoverNodeContext(t.Context(), 24, node, ""); err != nil {
			t.Fatalf("BUILDKITE_GHA_TEST_NODE24 is not Node 24: %v", err)
		}
		return node
	}
	if mise, err := exec.LookPath("mise"); err == nil {
		output, err := exec.Command(mise, "where", "node@24").CombinedOutput()
		if err == nil {
			node := filepath.Join(strings.TrimSpace(string(output)), "bin", "node")
			if _, err := discoverNodeContext(t.Context(), 24, node, ""); err == nil {
				return node
			}
		}
	}
	livePrerequisiteUnavailable(t, "Node 24 unavailable: set BUILDKITE_GHA_TEST_NODE24 or install managed Node 24 with `mise install node@24`")
	return ""
}

func requireDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		livePrerequisiteUnavailable(t, "Docker unavailable: docker executable not found")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dockerConfig := t.TempDir()
	if err := os.Chmod(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	dockerEnv := map[string]string{"DOCKER_CONFIG": dockerConfig}
	if output, err := boundedDockerCombinedOutput(ctx, dockerEnv, docker, "info", "--format", "{{.ServerVersion}}"); err != nil {
		livePrerequisiteUnavailable(t, "Docker unavailable: daemon probe failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := boundedDockerCombinedOutput(ctx, dockerEnv, docker, "buildx", "inspect", "default"); err != nil || dockerBuilderDriver(string(output)) != "docker" {
		livePrerequisiteUnavailable(t, "Docker unavailable: default Buildx builder is not the local docker driver: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return docker
}

func livePrerequisiteUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	message := fmt.Sprintf(format, args...)
	if os.Getenv("BUILDKITE_GHA_LIVE_REQUIRED") == "1" {
		t.Fatal(message)
	}
	t.Skip(message)
}
