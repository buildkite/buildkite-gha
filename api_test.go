package gha_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	gha "github.com/buildkite/buildkite-gha"
	"go.yaml.in/yaml/v4"
)

const apiJobID = "11111111-2222-4333-8444-555555555555"

type memoryBuildkite struct {
	files     map[string][]byte
	pipelines [][]byte
	failure   error
	metadata  error
}

func (b *memoryBuildkite) UploadArtifacts(ctx context.Context, root, _ string) error {
	return filepath.WalkDir(filepath.Join(root, ".buildkite-gha"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return b.UploadArtifactFrom(ctx, root, filepath.ToSlash(relative))
	})
}

func (b *memoryBuildkite) UploadArtifactFrom(_ context.Context, root, path string) error {
	if b.failure != nil {
		return b.failure
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err == nil {
		b.files[path] = data
	}
	return err
}

func (b *memoryBuildkite) DownloadArtifact(_ context.Context, path, destination, _ string) error {
	data, exists := b.files[path]
	if !exists {
		return fmt.Errorf("missing artifact %s", path)
	}
	path = filepath.Join(destination, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (*memoryBuildkite) SearchArtifactProducer(context.Context, string, string) (string, error) {
	return apiJobID, nil
}

func (b *memoryBuildkite) UploadPipeline(_ context.Context, pipeline []byte) error {
	if b.failure != nil {
		return b.failure
	}
	b.pipelines = append(b.pipelines, bytes.Clone(pipeline))
	return nil
}

func (*memoryBuildkite) SetMetadata(context.Context, string, string) error { return nil }
func (b *memoryBuildkite) GetMetadataBounded(context.Context, string, int) ([]byte, error) {
	if b.metadata != nil {
		return nil, b.metadata
	}
	return nil, gha.ErrMetadataUnavailable
}
func (*memoryBuildkite) GetStepAttribute(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("no existing step")
}
func (*memoryBuildkite) EnsureStepLabelSuffix(context.Context, string) error { return nil }
func (*memoryBuildkite) AnnotateJob(context.Context, string, string, string, string) error {
	return nil
}

type nativeCredentials struct{ redacted bool }

func (*nativeCredentials) ResolveSecret(_ context.Context, name string) (string, error) {
	if name != "GREETING" {
		return "", errors.New("undeclared secret")
	}
	return "native-secret", nil
}
func (c *nativeCredentials) AddRedaction(_ context.Context, value string) error {
	if value == "native-secret" {
		c.redacted = true
	}
	return nil
}
func (*nativeCredentials) GitCredentialHelper() (string, error) {
	return "", errors.New("repository credentials are not configured")
}

func apiFixture(t *testing.T, workflow string) (gha.Client, *memoryBuildkite, gha.CompileRequest) {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile("ci.yml", []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "ci.yml"}, {"-c", "user.name=API Test", "-c", "user.email=api@example.invalid", "commit", "--quiet", "-m", "Fixture"}} {
		if output, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	commit, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(commit))
	event := fmt.Sprintf(`{"provider":"github","event":"push","repository":{"owner":"owner","name":"repo","clone_url":"https://github.com/owner/repo.git","default_branch":"main"},"ref":"refs/heads/main","sha":%q,"actor":"api-test","payload":{"ref":"refs/heads/main"}}`, sha)
	if err := os.WriteFile("event.json", []byte(event), 0o600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "importer", "BUILDKITE_JOB_ID": apiJobID,
		"BUILDKITE_BUILD_ID": "22222222-2222-4333-8444-555555555555", "BUILDKITE_BUILD_NUMBER": "1",
		"BUILDKITE_COMMIT": sha, "BUILDKITE_BRANCH": "main", "BUILDKITE_REPO": "https://github.com/owner/repo.git",
		"BUILDKITE_BUILD_CHECKOUT_PATH": root, "BUILDKITE_AGENT_META_DATA_QUEUE": "hosted",
		"BUILDKITE_AGENT_ENDPOINT": "", "BUILDKITE_AGENT_ACCESS_TOKEN": "", "BUILDKITE_GHA_PLAN_DIGEST": "",
		"BUILDKITE_GHA_AGENT": filepath.Join(root, "no-agent"), "BUILDKITE_GHA_TELEMETRY_DISABLED": "true",
		"BUILDKITE_USE_REPOSITORY_PROVIDER_GIT_CREDENTIALS": "", "BUILDKITE_USE_GITHUB_APP_GIT_CREDENTIALS": "",
	} {
		t.Setenv(key, value)
	}
	// Any accidental transport subprocess fails the operation rather than
	// silently relying on a developer's installed agent.
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "buildkite-agent"), []byte("#!/bin/sh\nexit 91\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	backend := &memoryBuildkite{files: map[string][]byte{}}
	client := gha.Client{Version: "dev", Backend: backend, Credentials: &nativeCredentials{}}
	return client, backend, gha.CompileRequest{WorkflowPaths: []string{"ci.yml"}, EventPath: "event.json", DisableRunnerUser: true}
}

func TestCompileAndUploadPublishIdenticalBundles(t *testing.T) {
	for name, workflow := range map[string]string{
		"shell":    "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hello\n",
		"skipped":  "on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hello\n",
		"failure":  "on: push\njobs:\n  test:\n    runs-on: unconfigured-platform\n    steps:\n      - run: echo hello\n",
		"deferred": deferredAPIWorkflow,
	} {
		t.Run(name, func(t *testing.T) {
			client, backend, request := apiFixture(t, workflow)
			compiled, err := client.Compile(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if len(compiled.Pipeline) == 0 || len(backend.files) != 0 || len(backend.pipelines) != 0 {
				t.Fatal("Compile must return a pipeline without publishing it or its artifacts")
			}
			if err := client.Upload(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if len(backend.pipelines) != 1 || !bytes.Equal(compiled.Pipeline, backend.pipelines[0]) || len(compiled.Artifacts) != len(backend.files) {
				t.Fatal("Compile and Upload produced different bundles")
			}
			for _, artifact := range compiled.Artifacts {
				digest := fmt.Sprintf("sha256:%x", sha256.Sum256(artifact.Contents))
				if digest != artifact.Digest || !bytes.Equal(artifact.Contents, backend.files[artifact.Path]) {
					t.Fatalf("artifact %s changed between preparation and upload", artifact.Path)
				}
			}
		})
	}
}

type pipelineStep struct {
	Key     string         `yaml:"key"`
	Command string         `yaml:"command"`
	Steps   []pipelineStep `yaml:"steps"`
}

func commandSteps(t *testing.T, pipeline []byte) []pipelineStep {
	t.Helper()
	var decoded struct{ Steps []pipelineStep }
	if err := yaml.Unmarshal(pipeline, &decoded); err != nil {
		t.Fatal(err)
	}
	var commands []pipelineStep
	for _, group := range decoded.Steps {
		if group.Command != "" {
			commands = append(commands, group)
		}
		commands = append(commands, group.Steps...)
	}
	return commands
}

var planArgument = regexp.MustCompile(`--plan-digest '(sha256:[a-f0-9]{64})'`)

func runAPIStep(t *testing.T, client gha.Client, step pipelineStep) {
	t.Helper()
	match := planArgument.FindStringSubmatch(step.Command)
	if len(match) != 2 {
		t.Fatalf("step %s has no runtime plan argument", step.Key)
	}
	t.Setenv("BUILDKITE_STEP_KEY", step.Key)
	code, err := client.RunJob(t.Context(), gha.RunJobRequest{PlanDigest: match[1], PlanProducer: apiJobID, ArtifactProducer: apiJobID})
	if code != 0 || err != nil {
		t.Fatalf("RunJob = %d, %v", code, err)
	}
}

func TestRunJobUsesNativeSecretsAndPublishesResult(t *testing.T) {
	workflow := "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    env:\n      GREETING: ${{ secrets.GREETING }}\n    steps:\n      - run: test \"$GREETING\" = native-secret\n"
	client, backend, request := apiFixture(t, workflow)
	if err := client.Upload(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	runAPIStep(t, client, commandSteps(t, backend.pipelines[0])[0])
	if !client.Credentials.(*nativeCredentials).redacted {
		t.Fatal("secret was not registered with the native redactor")
	}
	for path, data := range backend.files {
		if strings.Contains(path, "/results/") {
			var result struct{ Result string }
			if err := json.Unmarshal(data, &result); err != nil || result.Result != "success" {
				t.Fatalf("published result = %s, %v", data, err)
			}
			return
		}
	}
	t.Fatal("runtime did not publish its authoritative result")
}

func TestRunJobPreservesExitCodesAndCancelledPublication(t *testing.T) {
	for _, test := range []struct {
		name      string
		tolerated bool
		cancelled bool
		code      int
	}{
		{name: "failed", code: 1},
		{name: "tolerated", tolerated: true, code: 78},
		{name: "cancelled", cancelled: true, code: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow := fmt.Sprintf("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    continue-on-error: %t\n    steps:\n      - run: exit 7\n", test.tolerated)
			client, backend, request := apiFixture(t, workflow)
			if err := client.Upload(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			step := commandSteps(t, backend.pipelines[0])[0]
			digest := planArgument.FindStringSubmatch(step.Command)[1]
			path := ".buildkite-gha/plans/" + strings.TrimPrefix(digest, "sha256:") + ".json"
			if err := os.WriteFile("plan.json", backend.files[path], 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BUILDKITE_STEP_KEY", step.Key)
			t.Setenv("BUILDKITE_GHA_PLAN_DIGEST", digest)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelled {
				cancel()
			}
			code, err := client.RunJob(ctx, gha.RunJobRequest{PlanPath: "plan.json", ArtifactProducer: apiJobID})
			if code != test.code || err == nil || test.cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("RunJob = %d, %v; want code %d and original cause", code, err, test.code)
			}
			for path := range backend.files {
				if strings.Contains(path, "/results/") {
					return
				}
			}
			t.Fatal("failed or cancelled job did not publish a terminal result")
		})
	}
}

const deferredAPIWorkflow = `on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"target":"first"},{"target":"second"}]' >> "$GITHUB_OUTPUT"
  build:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.target }}"
`

func TestUploadStageExpandsPublishedProducerOutput(t *testing.T) {
	client, backend, request := apiFixture(t, deferredAPIWorkflow)
	if err := client.Upload(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	stageArgument := regexp.MustCompile(`--stage-digest '(sha256:[a-f0-9]{64})'`)
	var stage gha.StageRequest
	for _, step := range commandSteps(t, backend.pipelines[0]) {
		if match := stageArgument.FindStringSubmatch(step.Command); len(match) == 2 {
			stage = gha.StageRequest{Digest: match[1], Producer: apiJobID}
		} else {
			runAPIStep(t, client, step)
		}
	}
	if stage.Digest == "" {
		t.Fatal("importer did not emit a deferred stage")
	}
	t.Setenv("BUILDKITE_STEP_KEY", "stage")
	if err := client.UploadStage(t.Context(), stage); err != nil {
		t.Fatal(err)
	}
	steps := commandSteps(t, backend.pipelines[len(backend.pipelines)-1])
	if len(steps) != 2 {
		t.Fatalf("deferred upload produced %d jobs, want 2", len(steps))
	}
	for _, step := range steps {
		runAPIStep(t, client, step)
	}
}

func TestTypedOperationsPreserveFailureCauses(t *testing.T) {
	client, backend, request := apiFixture(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	for _, paths := range [][]string{nil, {"missing.yml"}} {
		invalid := request
		invalid.WorkflowPaths = paths
		compiled, err := client.Compile(t.Context(), invalid)
		if err == nil || len(compiled.Pipeline) != 0 || len(compiled.Artifacts) != 0 {
			t.Fatalf("Compile with paths %v = %+v, %v; want empty result and error", paths, compiled, err)
		}
		if err := client.Upload(t.Context(), invalid); err == nil {
			t.Fatalf("Upload with paths %v succeeded", paths)
		}
		if len(backend.files) != 0 || len(backend.pipelines) != 0 {
			t.Fatalf("invalid paths %v published artifacts or a pipeline", paths)
		}
	}
	backend.failure = errors.New("native upload unavailable")
	if err := client.Upload(t.Context(), request); !errors.Is(err, backend.failure) {
		t.Fatalf("Upload lost its cause: %v", err)
	}
	backend.metadata = errors.New("metadata forbidden")
	request.EventPath = ""
	if _, err := client.Compile(t.Context(), request); !errors.Is(err, backend.metadata) {
		t.Fatalf("Compile hid a metadata error: %v", err)
	}
	backend.metadata = gha.ErrMetadataUnavailable
	if _, err := client.Compile(t.Context(), request); err != nil {
		t.Fatalf("confirmed webhook absence did not fall back: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Compile(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Compile lost cancellation: %v", err)
	}
	if err := client.UploadStage(ctx, gha.StageRequest{Digest: "sha256:" + strings.Repeat("a", 64), Producer: apiJobID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("UploadStage lost cancellation: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestAgentAPITransportIsInvocationLocal(t *testing.T) {
	client, _, request := apiFixture(t, "on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "https://agent.invalid/v3")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	request.Runners = map[string]gha.Runner{"ubuntu-latest": {Queue: "native"}}
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			local := client
			local.AgentAPI = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("Authorization") != "Token job-token" || req.URL.Path != "/v3/jobs/"+apiJobID+"/github-actions/runners" {
					t.Fatal("transport received an unauthenticated or out-of-scope request")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://agent.invalid/other-job"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})
			if _, err := local.Compile(t.Context(), request); err == nil || calls != 1 {
				t.Fatalf("Compile = %v, requests = %d; want rejection without following redirects", err, calls)
			}
		})
	}
}
