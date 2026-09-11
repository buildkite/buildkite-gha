package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	buildkitepipeline "github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"go.yaml.in/yaml/v4"
)

const (
	continueImporterJobID     = "0192f7d0-0000-4000-8000-00000000aaa1"
	continueProducerJobID     = "0192f7d0-0000-4000-8000-00000000bbb1"
	continueContinuationJobID = "0192f7d0-0000-4000-8000-00000000ccc1"
	continueBuildID           = "0192f7d0-0000-4000-8000-00000000ddd1"
)

// continueDeferredMatrixWorkflow has a producer, a consumer whose runs-on and
// matrix both come from the producer's output, a job that needs the consumer,
// and an unrelated job, so the deferred subgraph is build and publish only.
const continueDeferredMatrixWorkflow = `on: push
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: true
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"target":"linux","runner":"ubuntu-latest"}]' >> "$GITHUB_OUTPUT"
  build:
    needs: plan
    runs-on: ${{ matrix.runner }}
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.target }}"
  publish:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - run: true
`

// continueInitialUpload is one continuation the importer's upload wrote for
// the deferred-matrix workflow, with the pipeline and plans of that upload.
type continueInitialUpload struct {
	artifactPath string
	digest       string
	data         []byte
	artifact     continuationArtifact
	eventPath    string
	eventData    []byte
	pipeline     string
	plans        map[string][]byte
}

// artifactData returns the artifacts a continuation reads from the importer:
// its own continuation file and the event source the upload shares between
// every continuation.
func (initial continueInitialUpload) artifactData() map[string][]byte {
	return map[string][]byte{initial.artifactPath: initial.data, initial.eventPath: initial.eventData}
}

func runContinueInitialUpload(t *testing.T, uploadArgs ...string) continueInitialUpload {
	t.Helper()
	continuations := runContinueInitialUploads(t, continueDeferredMatrixWorkflow, uploadArgs...)
	if len(continuations) != 1 {
		t.Fatalf("upload wrote %d continuations, want 1", len(continuations))
	}
	return continuations[0]
}

// runContinueInitialUploads runs the importer's upload for a workflow inside
// a checkout and returns every continuation it wrote, ordered by consumer job.
func runContinueInitialUploads(t *testing.T, source string, uploadArgs ...string) []continueInitialUpload {
	t.Helper()
	return runContinueInitialUploadsForEvent(t, source, pushEventPath(t), uploadArgs...)
}

func runContinueInitialUploadsForEvent(t *testing.T, source, eventPath string, uploadArgs ...string) []continueInitialUpload {
	t.Helper()
	workflows := writeCommittedUploadWorkflows(t, map[string]string{"build.yml": source})
	return runContinueInitialUploadsInCheckout(t, workflows, eventPath, uploadArgs...)
}

// runContinueInitialUploadsInCheckout runs the importer's upload from the
// current directory, which the caller has made a committed checkout.
func runContinueInitialUploadsInCheckout(t *testing.T, workflows []string, eventPath string, uploadArgs ...string) []continueInitialUpload {
	t.Helper()
	requireImporterHost(t)
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_STEP_KEY", "continue-importer")
	t.Setenv("BUILDKITE_JOB_ID", continueImporterJobID)
	runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
	var stdout, stderr bytes.Buffer
	args := append([]string{"upload", "--event-path", eventPath}, uploadArgs...)
	if code := run(append(args, workflows...), &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("upload code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	pipeline := string(runner.commands[len(runner.commands)-1].stdin)
	plans := map[string][]byte{}
	var events []string
	var continuations []continueInitialUpload
	for path, contents := range runner.uploaded {
		switch {
		case strings.HasPrefix(path, ".buildkite-gha/continuations/events/"):
			events = append(events, path)
			if got := strings.TrimPrefix(transport.Digest(contents), "sha256:"); !strings.HasSuffix(path, "/"+got+".json") {
				t.Fatalf("event artifact %s is not addressed by its digest %s", path, got)
			}
		case strings.HasPrefix(path, ".buildkite-gha/continuations/"):
			artifact, err := decodeContinuationArtifact(contents, "dev")
			if err != nil {
				t.Fatal(err)
			}
			digest := transport.Digest(contents)
			if !strings.Contains(pipeline, "--continuation-digest '"+digest+"'") {
				t.Fatalf("pipeline does not reference continuation %s:\n%s", digest, pipeline)
			}
			continuations = append(continuations, continueInitialUpload{artifactPath: path, digest: digest, data: contents, artifact: artifact, pipeline: pipeline, plans: plans})
		case strings.HasPrefix(path, ".buildkite-gha/plans/"):
			plans[path] = contents
		}
	}
	if len(continuations) == 0 {
		t.Fatalf("upload wrote no continuation: %v", slices.Sorted(maps.Keys(runner.uploaded)))
	}
	if len(events) != 1 {
		t.Fatalf("upload wrote %d event artifacts, want exactly one shared by every continuation: %v", len(events), events)
	}
	for i := range continuations {
		eventPath, err := buildkitepipeline.ContinuationEventPath(continuations[i].artifact.Event.Digest)
		if err != nil {
			t.Fatal(err)
		}
		if eventPath != events[0] {
			t.Fatalf("continuation %s references event %s, upload wrote %s", continuations[i].digest, eventPath, events[0])
		}
		continuations[i].eventPath = eventPath
		continuations[i].eventData = runner.uploaded[eventPath]
	}
	slices.SortFunc(continuations, func(a, b continueInitialUpload) int {
		return strings.Compare(a.artifact.Continuation.Descriptor.Job, b.artifact.Continuation.Descriptor.Job)
	})
	return continuations
}

// producerManifest is the verified result the plan job would publish.
func (initial continueInitialUpload) producerManifest(t *testing.T, result, matrix string) []byte {
	t.Helper()
	return initial.producerManifestFrom(t, continueProducerJobID, result, matrix)
}

func (initial continueInitialUpload) producerManifestFrom(t *testing.T, jobID, result, matrix string) []byte {
	t.Helper()
	manifest := transport.ResultManifest{
		PlanDigest: initial.artifact.Producer.PlanDigest,
		Producer:   transport.Producer{BuildID: continueBuildID, JobID: jobID, StepKey: initial.artifact.Producer.StepKey},
		Result:     result,
	}
	if matrix != "" {
		manifest.Outputs = []transport.Output{{Name: "matrix", Value: matrix}}
	}
	encoded, err := transport.MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// continueRunner is a Buildkite agent that holds the importer's continuation
// and the producer's published result.
func (initial continueInitialUpload) continueRunner(manifest []byte) *cliCaptureRunner {
	return &cliCaptureRunner{
		jobByStep:  map[string]string{initial.artifact.Producer.StepKey: continueProducerJobID},
		dataByPath: initial.dataByPath(manifest),
	}
}

// dataByPath is artifactData plus the producer's published result.
func (initial continueInitialUpload) dataByPath(manifest []byte) map[string][]byte {
	data := initial.artifactData()
	data[transport.ResultPath(initial.artifact.Producer.StepKey, initial.artifact.Producer.PlanDigest)] = manifest
	return data
}

func runContinue(t *testing.T, runner *cliCaptureRunner, digest string) (int, string, string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", continueBuildID)
	t.Setenv("BUILDKITE_JOB_ID", continueContinuationJobID)
	var stdout, stderr bytes.Buffer
	code := run([]string{"continue", "--continuation-digest", digest, "--continuation-producer", continueImporterJobID}, &stdout, &stderr, "dev", runner)
	return code, stdout.String(), stderr.String()
}

type continuePipelineStep struct {
	Key       string `yaml:"key"`
	Label     string `yaml:"label"`
	Command   string `yaml:"command"`
	Skip      string `yaml:"skip"`
	Agents    struct{ Queue string }
	DependsOn []struct {
		Step string `yaml:"step"`
	} `yaml:"depends_on"`
	Notify []struct {
		GitHubCheck struct {
			Name string `yaml:"name"`
		} `yaml:"github_check"`
	} `yaml:"notify"`
}

func decodeContinuePipeline(t *testing.T, source []byte) (group string, key string, steps []continuePipelineStep) {
	t.Helper()
	var pipeline struct {
		Steps []struct {
			Group string                 `yaml:"group"`
			Key   string                 `yaml:"key"`
			Steps []continuePipelineStep `yaml:"steps"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(source, &pipeline); err != nil {
		t.Fatalf("pipeline YAML: %v\n%s", err, source)
	}
	if len(pipeline.Steps) != 1 {
		t.Fatalf("pipeline groups = %d, want 1:\n%s", len(pipeline.Steps), source)
	}
	return pipeline.Steps[0].Group, pipeline.Steps[0].Key, pipeline.Steps[0].Steps
}

func lastPipelineUpload(t *testing.T, runner *cliCaptureRunner) []byte {
	t.Helper()
	for i := len(runner.commands) - 1; i >= 0; i-- {
		if slices.Equal(runner.commands[i].args, []string{"pipeline", "upload", "--no-interpolation"}) {
			return runner.commands[i].stdin
		}
	}
	t.Fatalf("no pipeline upload in %#v", runner.commands)
	return nil
}

func pipelineUploads(runner *cliCaptureRunner) int {
	uploads := 0
	for _, command := range runner.commands {
		if slices.Equal(command.args, []string{"pipeline", "upload", "--no-interpolation"}) {
			uploads++
		}
	}
	return uploads
}

// TestContinueExpandsNeedsDerivedMatrix runs the importer's upload and then the
// deferred step against the producer's verified output. The expanded jobs use
// the importer's explicit runner mapping, the job that needs the consumer
// depends on every instance, and nothing the initial upload created is
// uploaded again.
func TestContinueExpandsNeedsDerivedMatrix(t *testing.T) {
	initial := runContinueInitialUpload(t, "--runner-queue", "ubuntu-latest=custom-linux")
	prefix := strings.TrimSuffix(initial.artifact.Producer.StepKey, "plan")
	if initial.artifact.Producer.StepKey != prefix+"plan" || !slices.Equal(initial.artifact.Continuation.Jobs, []string{"build", "publish"}) || initial.artifact.Continuation.StepKey != prefix+"build-matrix" {
		t.Fatalf("continuation = %+v", initial.artifact.Continuation)
	}
	if strings.Contains(initial.pipeline, `key: "`+prefix+`publish"`) || strings.Contains(initial.pipeline, `key: "`+prefix+`build"`) {
		t.Fatalf("initial upload created deferred jobs:\n%s", initial.pipeline)
	}
	initialSteps := 0
	for _, key := range []string{prefix + "lint", prefix + "plan", prefix + "build-matrix"} {
		if strings.Contains(initial.pipeline, `key: "`+key+`"`) {
			initialSteps++
		}
	}
	if initialSteps != 3 {
		t.Fatalf("initial upload is missing static or deferred-upload steps:\n%s", initial.pipeline)
	}

	matrix := `[{"target":"amd64","runner":"ubuntu-latest"},{"target":"arm64","runner":"ubuntu-latest"}]`
	runner := initial.continueRunner(initial.producerManifest(t, "success", matrix))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 {
		t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `Expanding job "build" into 2 matrix instances.`) || !strings.Contains(stdout, "Uploaded 3 jobs for \"build\"") {
		t.Fatalf("continue stdout = %q", stdout)
	}
	if pipelineUploads(runner) != 1 {
		t.Fatalf("pipeline uploads = %d: %#v", pipelineUploads(runner), runner.commands)
	}
	if !slices.Equal(runner.commands[0].args, []string{"artifact", "download", initial.artifactPath, runner.commands[0].args[3], "--step", continueImporterJobID}) {
		t.Fatalf("continuation was not read from the importer: %#v", runner.commands[0])
	}

	group, groupKey, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	if group != ":github: workflow · "+initial.artifact.Workflow.GroupLabel || groupKey != "" {
		t.Fatalf("deferred group = %q key %q, want the workflow group without a key", group, groupKey)
	}
	if len(steps) != 3 {
		t.Fatalf("deferred steps = %d, want two build instances and publish:\n%s", len(steps), lastPipelineUpload(t, runner))
	}
	var buildKeys []string
	var publish continuePipelineStep
	for _, step := range steps {
		if step.Key == prefix+"publish" {
			publish = step
			continue
		}
		if !strings.HasPrefix(step.Key, prefix+"build-") || step.Key == initial.artifact.Continuation.StepKey {
			t.Fatalf("unexpected deferred step %q", step.Key)
		}
		buildKeys = append(buildKeys, step.Key)
		if step.Agents.Queue != "custom-linux" {
			t.Fatalf("build instance %q runs on queue %q, want the importer's explicit mapping", step.Key, step.Agents.Queue)
		}
		if len(step.DependsOn) != 1 || step.DependsOn[0].Step != prefix+"plan" {
			t.Fatalf("build instance %q depends on %#v, want the existing plan step", step.Key, step.DependsOn)
		}
		if !strings.Contains(step.Command, "--step '"+continueImporterJobID+"'") || !strings.Contains(step.Command, "--step '"+continueContinuationJobID+"'") {
			t.Fatalf("build instance %q must load the distribution from the importer and its plan from the continuation:\n%s", step.Key, step.Command)
		}
		if len(step.Notify) != 1 || !strings.HasSuffix(step.Notify[0].GitHubCheck.Name, ` / build (runner="ubuntu-latest", target="amd64") (push)`) && !strings.HasSuffix(step.Notify[0].GitHubCheck.Name, ` / build (runner="ubuntu-latest", target="arm64") (push)`) {
			t.Fatalf("build instance %q check = %#v", step.Key, step.Notify)
		}
	}
	if len(buildKeys) != 2 || publish.Key == "" {
		t.Fatalf("deferred steps = %#v", steps)
	}
	var publishNeeds []string
	for _, dependency := range publish.DependsOn {
		publishNeeds = append(publishNeeds, dependency.Step)
	}
	slices.Sort(publishNeeds)
	slices.Sort(buildKeys)
	if !slices.Equal(publishNeeds, buildKeys) {
		t.Fatalf("publish depends on %v, want the build instances %v", publishNeeds, buildKeys)
	}

	plans := uploadedPlans(t, runner)
	if len(plans["build"]) != 2 || len(plans["publish"]) != 1 || len(plans) != 2 {
		t.Fatalf("continuation uploaded plans for %v, want build and publish only", slices.Sorted(maps.Keys(plans)))
	}
	for _, jobPlan := range plans["build"] {
		if jobPlan.Target.Queue != "custom-linux" {
			t.Fatalf("build plan targets queue %q", jobPlan.Target.Queue)
		}
	}
	for path := range runner.uploaded {
		if _, initialPlan := initial.plans[path]; initialPlan {
			t.Fatalf("continuation re-uploaded plan %s from the initial upload", path)
		}
	}
}

// TestContinueRejectsRowsThatEscapeThePolicy proves producer output is
// untrusted graph input: rows cannot route jobs to runners the importer did
// not map or to platforms it has no runtime for, cannot smuggle settings
// through unused keys, and cannot bypass the static matrix validation.
func TestContinueRejectsRowsThatEscapeThePolicy(t *testing.T) {
	initial := runContinueInitialUpload(t, "--runner-queue", "ubuntu-latest=custom-linux")
	for _, test := range []struct {
		name, matrix, want string
	}{
		{name: "unmapped runner", matrix: `[{"target":"x","runner":"self-hosted-privileged"}]`, want: "compile deferred jobs"},
		{name: "platform without runtime", matrix: `[{"target":"x","runner":"macos-latest"}]`, want: "no runtime distribution configured for darwin/arm64"},
		{name: "wrong shape", matrix: `{"runner":["ubuntu-latest"]}`, want: "is invalid"},
		{name: "wrong row type", matrix: `["ubuntu-latest"]`, want: "is invalid"},
		{name: "no rows", matrix: `[]`, want: "expanded to no matrix instances"},
		{name: "output missing", matrix: "", want: `did not publish output "matrix"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := initial.continueRunner(initial.producerManifest(t, "success", test.matrix))
			code, stdout, stderr := runContinue(t, runner, initial.digest)
			if code != 1 || !strings.Contains(stderr, test.want) || !strings.Contains(stderr, continueRetryGuidance) {
				t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if pipelineUploads(runner) != 0 || len(runner.uploaded) != 0 {
				t.Fatalf("rejected matrix reached Buildkite: %#v", runner.commands)
			}
		})
	}

	t.Run("unused row keys change nothing", func(t *testing.T) {
		matrix := `[{"target":"x","runner":"ubuntu-latest","runs-on":"self-hosted","permissions":"write-all","queue":"privileged"}]`
		runner := initial.continueRunner(initial.producerManifest(t, "success", matrix))
		code, stdout, stderr := runContinue(t, runner, initial.digest)
		if code != 0 {
			t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		plans := uploadedPlans(t, runner)
		if len(plans["build"]) != 1 || plans["build"][0].Target.Queue != "custom-linux" || plans["build"][0].GitHubToken != nil {
			t.Fatalf("build plan = %+v", plans["build"])
		}
		// Row values appear in instance labels, as they do for static
		// matrices, but never in the agent targeting.
		_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
		for _, step := range steps {
			if step.Agents.Queue != "custom-linux" {
				t.Fatalf("deferred step %q targets queue %q: %#v", step.Key, step.Agents.Queue, step)
			}
		}
	})
}

// TestContinueVerifiesItsInputs proves the deferred step trusts only the
// continuation the named importer wrote and only the workflow it compiled.
func TestContinueVerifiesItsInputs(t *testing.T) {
	initial := runContinueInitialUpload(t)
	manifest := initial.producerManifest(t, "success", `[{"target":"x","runner":"ubuntu-latest"}]`)

	t.Run("tampered continuation", func(t *testing.T) {
		runner := initial.continueRunner(manifest)
		runner.dataByPath[initial.artifactPath] = bytes.Replace(initial.data, []byte(`"resolved"`), []byte(`"resolveD"`), 1)
		code, _, stderr := runContinue(t, runner, initial.digest)
		if code != 1 || !strings.Contains(stderr, "does not match expected") || pipelineUploads(runner) != 0 {
			t.Fatalf("continue code = %d, stderr = %q", code, stderr)
		}
	})

	t.Run("continuation from another importer", func(t *testing.T) {
		runner := initial.continueRunner(manifest)
		var stdout, stderr bytes.Buffer
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_BUILD_ID", continueBuildID)
		t.Setenv("BUILDKITE_JOB_ID", continueContinuationJobID)
		code := run([]string{"continue", "--continuation-digest", initial.digest, "--continuation-producer", continueProducerJobID}, &stdout, &stderr, "dev", runner)
		if code != 1 || !strings.Contains(stderr.String(), "was written by importer") || pipelineUploads(runner) != 0 {
			t.Fatalf("continue code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("workflow changed in the checkout", func(t *testing.T) {
		path := filepath.Join(".github", "workflows", "build.yml")
		if err := os.WriteFile(path, []byte(strings.Replace(continueDeferredMatrixWorkflow, "run: true", "run: false", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(path, []byte(continueDeferredMatrixWorkflow), 0o600) })
		runner := initial.continueRunner(manifest)
		code, _, stderr := runContinue(t, runner, initial.digest)
		if code != 1 || !strings.Contains(stderr, "differs from the workflow the importer compiled") || pipelineUploads(runner) != 0 {
			t.Fatalf("continue code = %d, stderr = %q", code, stderr)
		}
	})

	t.Run("outside Buildkite", func(t *testing.T) {
		runner := initial.continueRunner(manifest)
		t.Setenv("BUILDKITE", "true")
		t.Setenv("BUILDKITE_BUILD_ID", continueBuildID)
		t.Setenv("BUILDKITE_JOB_ID", "")
		var stdout, stderr bytes.Buffer
		code := run([]string{"continue", "--continuation-digest", initial.digest, "--continuation-producer", continueImporterJobID}, &stdout, &stderr, "dev", runner)
		if code != 2 || !strings.Contains(stderr.String(), "BUILDKITE_JOB_ID") || len(runner.commands) != 0 {
			t.Fatalf("continue code = %d, stderr = %q, commands = %#v", code, stderr.String(), runner.commands)
		}
	})
}

// TestContinueFindsProducerWithOneRowMatrix covers a producer whose single
// instance comes from a one-row static matrix: its step key carries a matrix
// digest, and the continuation must read the result from that instance.
func TestContinueFindsProducerWithOneRowMatrix(t *testing.T) {
	source := strings.Replace(continueDeferredMatrixWorkflow, `  plan:
    runs-on: ubuntu-latest
`, `  plan:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest]
`, 1)
	if source == continueDeferredMatrixWorkflow {
		t.Fatal("workflow fixture did not change")
	}
	initial := runContinueInitialUploads(t, source)[0]
	producerKey := initial.artifact.Producer.StepKey
	logicalKey := compiler.LogicalJobStepKey(initial.artifact.Workflow.Namespace, "plan")
	if producerKey == logicalKey || !strings.HasPrefix(producerKey, logicalKey+"-") {
		t.Fatalf("producer step key = %q, want the matrix instance key under %q", producerKey, logicalKey)
	}
	if !strings.Contains(initial.pipeline, `key: "`+producerKey+`"`) || strings.Contains(initial.pipeline, `key: "`+logicalKey+`"`) {
		t.Fatalf("initial upload must create the producer instance %q only:\n%s", producerKey, initial.pipeline)
	}
	_, _, initialSteps := decodeContinuePipeline(t, []byte(initial.pipeline))
	for _, step := range initialSteps {
		if step.Key != initial.artifact.Continuation.StepKey {
			continue
		}
		if len(step.DependsOn) != 1 || step.DependsOn[0].Step != producerKey {
			t.Fatalf("deferred step depends on %#v, want the producer instance %q", step.DependsOn, producerKey)
		}
	}

	runner := initial.continueRunner(initial.producerManifest(t, "success", `[{"target":"amd64","runner":"ubuntu-latest"}]`))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 || !strings.Contains(stdout, "Uploaded 2 jobs for \"build\"") {
		t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	for _, step := range steps {
		if strings.HasPrefix(step.Key, logicalKey) {
			t.Fatalf("continuation re-uploaded the producer %q", step.Key)
		}
		if step.Key != compiler.LogicalJobStepKey(initial.artifact.Workflow.Namespace, "publish") && (len(step.DependsOn) != 1 || step.DependsOn[0].Step != producerKey) {
			t.Fatalf("deferred job %q depends on %#v, want the producer instance %q", step.Key, step.DependsOn, producerKey)
		}
	}
}

// TestContinuePinsDeferredActions proves the initial upload resolves the
// actions of deferred jobs, records their locks in the continuation, and the
// deferred upload refuses an action whose source changed since then.
func TestContinuePinsDeferredActions(t *testing.T) {
	source := strings.Replace(continueDeferredMatrixWorkflow, `      - run: echo "${{ matrix.target }}"
`, `      - uses: ./.github/actions/deferred
`, 1)
	if source == continueDeferredMatrixWorkflow {
		t.Fatal("workflow fixture did not change")
	}
	eventPath := pushEventPath(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{"build.yml": source})
	actionPath := filepath.Join(repository, ".github", "actions", "deferred", "action.yml")
	if err := os.MkdirAll(filepath.Dir(actionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction := func(step string) {
		t.Helper()
		if err := os.WriteFile(actionPath, []byte("name: deferred\nruns:\n  using: composite\n  steps:\n    - run: "+step+"\n      shell: bash\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAction("true")
	if output, err := exec.Command("git", "-C", repository, "add", ".github/actions").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commitUploadWorkflows(t, repository)
	workflows := enterUploadWorkflows(t, repository, map[string]string{"build.yml": source})
	initial := runContinueInitialUploadsInCheckout(t, workflows, eventPath)[0]

	locks := initial.artifact.Continuation.ActionLocks
	if len(locks) != 1 || locks[0].Source != "workspace" || locks[0].Path != ".github/actions/deferred" || locks[0].SourceDigest == "" {
		t.Fatalf("continuation action locks = %#v", locks)
	}
	manifest := initial.producerManifest(t, "success", `[{"target":"linux","runner":"ubuntu-latest"}]`)

	t.Run("unchanged action", func(t *testing.T) {
		runner := initial.continueRunner(manifest)
		code, stdout, stderr := runContinue(t, runner, initial.digest)
		if code != 0 || !strings.Contains(stdout, "Uploaded 2 jobs for \"build\"") {
			t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		for _, job := range uploadedPlans(t, runner)["build"] {
			if len(job.Actions) != 1 || !reflect.DeepEqual(job.Actions[0], locks[0]) {
				t.Fatalf("deferred plan actions = %#v, want the recorded lock %#v", job.Actions, locks[0])
			}
		}
	})

	t.Run("changed action", func(t *testing.T) {
		writeAction("false")
		t.Cleanup(func() { writeAction("true") })
		runner := initial.continueRunner(manifest)
		code, _, stderr := runContinue(t, runner, initial.digest)
		if code != 1 || !strings.Contains(stderr, "which the initial upload did not resolve to that revision") || pipelineUploads(runner) != 0 {
			t.Fatalf("continue code = %d, stderr = %q", code, stderr)
		}
	})
}

// TestContinueRecordsVarsReferencedOnlyByDeferredActions proves the importer
// resolves repository and organization variables when the only vars
// reference lives in an action of a deferred job, records the scopes in the
// continuation, and the deferred step compiles with them without a request
// of its own, so a deferred job sees the same variables as a static one.
func TestContinueRecordsVarsReferencedOnlyByDeferredActions(t *testing.T) {
	source := strings.Replace(continueDeferredMatrixWorkflow, `      - run: echo "${{ matrix.target }}"
`, `      - uses: ./.github/actions/region
`, 1)
	if source == continueDeferredMatrixWorkflow {
		t.Fatal("workflow fixture did not change")
	}
	variables, variableRequests := agentVariablesHandler(t, http.StatusOK, "")
	agent, _ := agentStub(t, "job-secret", http.StatusOK, variables)
	setAgentResolutionEnvironment(t, agent.URL)
	eventPath := pushEventPath(t)
	repository := writeUploadWorkflowRepository(t, map[string]string{"build.yml": source})
	t.Chdir(repository)
	writeRegionAction(t, regionInputDefaultAction)
	if output, err := exec.Command("git", "-C", repository, "add", ".github/actions").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commitUploadWorkflows(t, repository)
	workflows := enterUploadWorkflows(t, repository, map[string]string{"build.yml": source})
	initial := runContinueInitialUploadsInCheckout(t, workflows, eventPath)[0]

	if *variableRequests != 1 {
		t.Fatalf("initial upload made %d variables requests, want 1", *variableRequests)
	}
	if recorded := initial.artifact.Vars; !recorded.Resolved || !maps.Equal(recorded.Repository, stubRepositoryVariables) || !maps.Equal(recorded.Organization, stubOrganizationVariables) {
		t.Fatalf("continuation recorded vars = %#v, want the resolved scopes", recorded)
	}
	staticPlans := 0
	for path, content := range initial.plans {
		job, err := plan.Decode(content)
		if err != nil {
			t.Fatalf("decode uploaded plan %s: %v", path, err)
		}
		staticPlans++
		if !maps.Equal(job.RepositoryVars, stubRepositoryVariables) || !maps.Equal(job.OrganizationVars, stubOrganizationVariables) {
			t.Fatalf("static plan %s vars = %#v / %#v, want the resolved scopes", job.Workflow.LogicalJobID, job.RepositoryVars, job.OrganizationVars)
		}
	}
	if staticPlans != 2 {
		t.Fatalf("initial upload wrote %d plans, want lint and plan", staticPlans)
	}

	runner := initial.continueRunner(initial.producerManifest(t, "success", `[{"target":"linux","runner":"ubuntu-latest"}]`))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 || !strings.Contains(stdout, "Uploaded 2 jobs for \"build\"") {
		t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if *variableRequests != 1 {
		t.Fatalf("deferred step made %d variables requests of its own", *variableRequests-1)
	}
	plans := uploadedPlans(t, runner)
	if len(plans["build"]) != 1 || !maps.Equal(plans["build"][0].RepositoryVars, stubRepositoryVariables) || !maps.Equal(plans["build"][0].OrganizationVars, stubOrganizationVariables) {
		t.Fatalf("deferred plans = %#v, want the recorded scopes", plans)
	}
	assertNoVariableValueLeak(t, runner, stdout, stderr)
}

// continueTwoMatricesWorkflow has two independent producer and consumer
// pairs, so the initial upload creates two deferred steps that each expand
// one matrix while the other stays deferred.
const continueTwoMatricesWorkflow = `on: push
jobs:
  plan-a:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"target":"a"}]' >> "$GITHUB_OUTPUT"
  build-a:
    needs: plan-a
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan-a.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.target }}"
  plan-b:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"target":"b"}]' >> "$GITHUB_OUTPUT"
  build-b:
    needs: plan-b
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan-b.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.target }}"
  publish-b:
    needs: build-b
    runs-on: ubuntu-latest
    steps:
      - run: true
`

// TestContinueExpandsIndependentMatrices proves a workflow with two
// needs-derived matrices gets one deferred step each, every artifact records
// the other continuation, and both continuations expand while the other is
// still deferred instead of being reported as drift.
func TestContinueExpandsIndependentMatrices(t *testing.T) {
	continuations := runContinueInitialUploads(t, continueTwoMatricesWorkflow)
	if len(continuations) != 2 {
		t.Fatalf("upload wrote %d continuations, want 2", len(continuations))
	}
	a, b := continuations[0], continuations[1]
	if a.artifact.Continuation.Descriptor.Job != "build-a" || b.artifact.Continuation.Descriptor.Job != "build-b" || !slices.Equal(b.artifact.Continuation.Jobs, []string{"build-b", "publish-b"}) {
		t.Fatalf("continuations = %+v, %+v", a.artifact.Continuation, b.artifact.Continuation)
	}
	if len(a.artifact.Others) != 1 || a.artifact.Others[0].StepKey != b.artifact.Continuation.StepKey || len(b.artifact.Others) != 1 || b.artifact.Others[0].StepKey != a.artifact.Continuation.StepKey {
		t.Fatalf("artifacts do not record each other: %+v / %+v", a.artifact.Others, b.artifact.Others)
	}
	for _, key := range []string{a.artifact.Continuation.StepKey, b.artifact.Continuation.StepKey, a.artifact.Producer.StepKey, b.artifact.Producer.StepKey} {
		if !strings.Contains(a.pipeline, `key: "`+key+`"`) {
			t.Fatalf("initial upload is missing step %q:\n%s", key, a.pipeline)
		}
	}

	const producerBJobID = "0192f7d0-0000-4000-8000-00000000bbb2"
	runner := &cliCaptureRunner{
		jobByStep: map[string]string{a.artifact.Producer.StepKey: continueProducerJobID, b.artifact.Producer.StepKey: producerBJobID},
		dataByPath: map[string][]byte{
			a.artifactPath: a.data,
			b.artifactPath: b.data,
			a.eventPath:    a.eventData,
			transport.ResultPath(a.artifact.Producer.StepKey, a.artifact.Producer.PlanDigest): a.producerManifest(t, "success", `[{"target":"a1"},{"target":"a2"}]`),
			transport.ResultPath(b.artifact.Producer.StepKey, b.artifact.Producer.PlanDigest): b.producerManifestFrom(t, producerBJobID, "success", `[{"target":"b1"}]`),
		},
	}
	// build-a expands to two instances; build-b to one instance plus publish-b.
	for i, want := range []string{"Uploaded 2 jobs for \"build-a\"", "Uploaded 2 jobs for \"build-b\""} {
		continuation, other := continuations[i], continuations[1-i]
		consumer := continuation.artifact.Continuation.Descriptor.Job
		uploads := pipelineUploads(runner)
		code, stdout, stderr := runContinue(t, runner, continuation.digest)
		if code != 0 || !strings.Contains(stdout, want) || pipelineUploads(runner) != uploads+1 {
			t.Fatalf("continue %s code = %d, stdout = %q, stderr = %q", consumer, code, stdout, stderr)
		}
		_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
		if len(steps) != 2 {
			t.Fatalf("continue %s uploaded %d steps, want 2:\n%s", consumer, len(steps), lastPipelineUpload(t, runner))
		}
		for _, step := range steps {
			if !strings.HasPrefix(step.Key, compiler.LogicalJobStepKey(other.artifact.Workflow.Namespace, "")) {
				t.Fatalf("continue %s uploaded step %q outside the workflow namespace", consumer, step.Key)
			}
			for _, job := range other.artifact.Continuation.Jobs {
				if strings.HasPrefix(step.Key, compiler.LogicalJobStepKey(other.artifact.Workflow.Namespace, job)) {
					t.Fatalf("continue %s uploaded %q, which belongs to the other continuation", consumer, step.Key)
				}
			}
		}
	}
}

// TestContinueEnforcesSharedJobBudget proves that the deferred uploads of one
// workflow cannot together grow the build past the graph bound: ten consumers
// of one producer each get a tenth of the jobs left after the static graph,
// recorded in their artifacts, and a producer output with one row more than
// that share fails the deferred step before it uploads anything.
func TestContinueEnforcesSharedJobBudget(t *testing.T) {
	const consumers = 10
	var source strings.Builder
	source.WriteString(`on: push
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"t":1}]' >> "$GITHUB_OUTPUT"
`)
	for i := range consumers {
		fmt.Fprintf(&source, `  build-%d:
    needs: plan
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.t }}"
`, i)
	}
	continuations := runContinueInitialUploads(t, source.String())
	if len(continuations) != consumers {
		t.Fatalf("upload wrote %d continuations, want %d", len(continuations), consumers)
	}
	// plan is the only static job.
	budget := (compiler.MaxRuntimeMatrixGraphJobs - 1) / consumers
	for _, continuation := range continuations {
		if continuation.artifact.Continuation.JobBudget != budget {
			t.Fatalf("continuation %s job budget = %d, want %d", continuation.artifact.Continuation.StepKey, continuation.artifact.Continuation.JobBudget, budget)
		}
	}
	rows := func(count int) string {
		values := make([]string, 0, count)
		for i := range count {
			values = append(values, fmt.Sprintf(`{"t":%d}`, i))
		}
		return "[" + strings.Join(values, ",") + "]"
	}
	initial := continuations[0]

	t.Run("one row over the share", func(t *testing.T) {
		runner := initial.continueRunner(initial.producerManifest(t, "success", rows(budget+1)))
		code, _, stderr := runContinue(t, runner, initial.digest)
		want := fmt.Sprintf(`job "plan" produced %d matrix rows, but step %q may upload at most %d jobs`, budget+1, initial.artifact.Continuation.StepKey, budget)
		if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "share the 1024-job graph bound") || pipelineUploads(runner) != 0 {
			t.Fatalf("continue code = %d, stderr = %q, uploads = %d", code, stderr, pipelineUploads(runner))
		}
	})

	t.Run("exactly the share", func(t *testing.T) {
		runner := initial.continueRunner(initial.producerManifest(t, "success", rows(budget)))
		code, stdout, stderr := runContinue(t, runner, initial.digest)
		if code != 0 || !strings.Contains(stdout, fmt.Sprintf("Uploaded %d jobs", budget)) || pipelineUploads(runner) != 1 {
			t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if _, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner)); len(steps) != budget {
			t.Fatalf("continue uploaded %d steps, want %d", len(steps), budget)
		}
	})
}

// continueSharedGateWorkflow has two independent needs-derived matrices whose
// consumers deploy to the same protected environment, so both continuations
// need the same approval gate, which no static job creates.
const continueSharedGateWorkflow = `on: push
jobs:
  plan-a:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"target":"a"}]' >> "$GITHUB_OUTPUT"
  deploy-a:
    needs: plan-a
    runs-on: ubuntu-latest
    environment: production
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan-a.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.target }}"
  plan-b:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.matrix.outputs.matrix }}
    steps:
      - id: matrix
        run: echo 'matrix=[{"target":"b"}]' >> "$GITHUB_OUTPUT"
  deploy-b:
    needs: plan-b
    runs-on: ubuntu-latest
    environment: production
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan-b.outputs.matrix) }}
    steps:
      - run: echo "${{ matrix.target }}"
`

// TestContinueSharesApprovalGates proves how two continuations share one
// environment approval gate: the first to upload creates it, a later one that
// finds it in the build references it, and one that loses the race between
// checking and uploading retries once with the gate as an existing step
// instead of failing the build.
func TestContinueSharesApprovalGates(t *testing.T) {
	agent, _ := agentEnvironmentsStub(t, "job-secret", http.StatusOK)
	setAgentResolutionEnvironment(t, agent.URL)
	continuations := runContinueInitialUploads(t, continueSharedGateWorkflow)
	if len(continuations) != 2 {
		t.Fatalf("upload wrote %d continuations, want 2", len(continuations))
	}
	a, b := continuations[0], continuations[1]
	if len(a.artifact.ApprovalGates) != 0 || strings.Contains(a.pipeline, "block:") {
		t.Fatalf("initial upload created an approval gate no static job uses: %v\n%s", a.artifact.ApprovalGates, a.pipeline)
	}
	const producerBJobID = "0192f7d0-0000-4000-8000-00000000bbb2"
	newRunner := func() *cliCaptureRunner {
		return &cliCaptureRunner{
			jobByStep: map[string]string{a.artifact.Producer.StepKey: continueProducerJobID, b.artifact.Producer.StepKey: producerBJobID},
			dataByPath: map[string][]byte{
				a.artifactPath: a.data,
				b.artifactPath: b.data,
				a.eventPath:    a.eventData,
				transport.ResultPath(a.artifact.Producer.StepKey, a.artifact.Producer.PlanDigest): a.producerManifest(t, "success", `[{"target":"a1"}]`),
				transport.ResultPath(b.artifact.Producer.StepKey, b.artifact.Producer.PlanDigest): b.producerManifestFrom(t, producerBJobID, "success", `[{"target":"b1"}]`),
			},
			stepAttributes: map[string]map[string]string{},
		}
	}
	gateSteps := func(pipeline []byte) (created []string, referenced []string) {
		var decoded struct {
			Steps []struct {
				Steps []struct {
					Key       string `yaml:"key"`
					Block     string `yaml:"block"`
					DependsOn []struct {
						Step string `yaml:"step"`
					} `yaml:"depends_on"`
				} `yaml:"steps"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal(pipeline, &decoded); err != nil {
			t.Fatalf("pipeline YAML: %v\n%s", err, pipeline)
		}
		for _, group := range decoded.Steps {
			for _, step := range group.Steps {
				if step.Block != "" {
					created = append(created, step.Key)
				}
				for _, dependency := range step.DependsOn {
					if strings.Contains(dependency.Step, "approve") {
						referenced = append(referenced, dependency.Step)
					}
				}
			}
		}
		return created, referenced
	}

	// The first continuation finds no gate and creates it.
	runner := newRunner()
	code, stdout, stderr := runContinue(t, runner, a.digest)
	if code != 0 || !strings.Contains(stdout, `Uploaded 1 jobs for "deploy-a"`) {
		t.Fatalf("continue deploy-a code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	created, referenced := gateSteps(lastPipelineUpload(t, runner))
	if len(created) != 1 || len(referenced) != 1 || created[0] != referenced[0] {
		t.Fatalf("continue deploy-a created gates %v, referenced %v:\n%s", created, referenced, lastPipelineUpload(t, runner))
	}
	gate := created[0]

	// The second continuation finds the gate in the build and only references it.
	runner = newRunner()
	runner.stepAttributes[gate] = map[string]string{"key": gate}
	code, stdout, stderr = runContinue(t, runner, b.digest)
	if code != 0 || !strings.Contains(stdout, `Uploaded 1 jobs for "deploy-b"`) || pipelineUploads(runner) != 1 {
		t.Fatalf("continue deploy-b code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if created, referenced := gateSteps(lastPipelineUpload(t, runner)); len(created) != 0 || !slices.Equal(referenced, []string{gate}) {
		t.Fatalf("continue deploy-b created gates %v, referenced %v:\n%s", created, referenced, lastPipelineUpload(t, runner))
	}

	// Both checked before either uploaded: the first upload includes the gate
	// and is rejected because the other continuation created it meanwhile.
	runner = newRunner()
	runner.pipelineUploadHook = func(pipeline []byte) error {
		if created, _ := gateSteps(pipeline); len(created) != 0 {
			runner.stepAttributes[gate] = map[string]string{"key": gate}
			return errors.New("step key " + gate + " already exists in this build")
		}
		return nil
	}
	code, stdout, stderr = runContinue(t, runner, b.digest)
	if code != 0 || !strings.Contains(stdout, `created approval gates "`+gate+`" first`) || !strings.Contains(stdout, `Uploaded 1 jobs for "deploy-b"`) || pipelineUploads(runner) != 2 {
		t.Fatalf("continue deploy-b after gate race code = %d, uploads = %d, stdout = %q, stderr = %q", code, pipelineUploads(runner), stdout, stderr)
	}
	if created, referenced := gateSteps(lastPipelineUpload(t, runner)); len(created) != 0 || !slices.Equal(referenced, []string{gate}) {
		t.Fatalf("retried upload created gates %v, referenced %v:\n%s", created, referenced, lastPipelineUpload(t, runner))
	}

	// A rejection for any other reason still fails, with the gate untouched.
	runner = newRunner()
	runner.pipelineUploadErr = errors.New("step key gha-deploy-b-000000000000 already exists in this build")
	code, stdout, stderr = runContinue(t, runner, b.digest)
	if code == 0 || !strings.Contains(stderr, "already exists in this build") || pipelineUploads(runner) != 1 {
		t.Fatalf("continue deploy-b with a rejected upload code = %d, uploads = %d, stdout = %q, stderr = %q", code, pipelineUploads(runner), stdout, stderr)
	}
}

// continueMixedGateWorkflow adds to continueSharedGateWorkflow a deferred job
// behind a second environment only deploy-b's continuation uses, so that
// continuation creates a shared gate and a unique gate together.
const continueMixedGateWorkflow = continueSharedGateWorkflow + `  promote-b:
    needs: deploy-b
    runs-on: ubuntu-latest
    environment: staging
    steps:
      - run: true
`

// TestContinueRetriesWhenOnlySomeGatesRaced proves a continuation that lost
// the race for a shared gate still creates the gate only it uses: the retried
// upload references the gate another continuation created and keeps the
// missing one, rather than requiring every created gate to exist.
func TestContinueRetriesWhenOnlySomeGatesRaced(t *testing.T) {
	agent, _ := agentEnvironmentsStub(t, "job-secret", http.StatusOK)
	setAgentResolutionEnvironment(t, agent.URL)
	continuations := runContinueInitialUploads(t, continueMixedGateWorkflow)
	if len(continuations) != 2 {
		t.Fatalf("upload wrote %d continuations, want 2", len(continuations))
	}
	a, b := continuations[0], continuations[1]
	const producerBJobID = "0192f7d0-0000-4000-8000-00000000bbb2"
	runner := &cliCaptureRunner{
		jobByStep: map[string]string{a.artifact.Producer.StepKey: continueProducerJobID, b.artifact.Producer.StepKey: producerBJobID},
		dataByPath: map[string][]byte{
			a.artifactPath: a.data,
			b.artifactPath: b.data,
			a.eventPath:    a.eventData,
			transport.ResultPath(b.artifact.Producer.StepKey, b.artifact.Producer.PlanDigest): b.producerManifestFrom(t, producerBJobID, "success", `[{"target":"b1"}]`),
		},
		stepAttributes: map[string]map[string]string{},
	}
	// blockKeys returns the block steps a pipeline creates by key and label,
	// and every step key the pipeline depends on.
	blockKeys := func(pipeline []byte) (blocks map[string]string, referenced map[string]bool) {
		var decoded struct {
			Steps []struct {
				Steps []struct {
					Key       string `yaml:"key"`
					Block     string `yaml:"block"`
					DependsOn []struct {
						Step string `yaml:"step"`
					} `yaml:"depends_on"`
				} `yaml:"steps"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal(pipeline, &decoded); err != nil {
			t.Fatal(err)
		}
		blocks, referenced = map[string]string{}, map[string]bool{}
		for _, group := range decoded.Steps {
			for _, step := range group.Steps {
				if step.Block != "" {
					blocks[step.Key] = step.Block
				}
				for _, dependency := range step.DependsOn {
					referenced[dependency.Step] = true
				}
			}
		}
		return blocks, referenced
	}
	// The first upload creates both gates; another continuation created the
	// production gate meanwhile, so Buildkite rejects the upload.
	var productionGate string
	runner.pipelineUploadHook = func(pipeline []byte) error {
		blocks, _ := blockKeys(pipeline)
		for key, label := range blocks {
			if strings.Contains(label, "production") && runner.stepAttributes[key] == nil {
				productionGate = key
				runner.stepAttributes[key] = map[string]string{"key": key}
				return errors.New("step key " + key + " already exists in this build")
			}
		}
		return nil
	}
	code, stdout, stderr := runContinue(t, runner, b.digest)
	if code != 0 || productionGate == "" || !strings.Contains(stdout, `created approval gates "`+productionGate+`" first`) || !strings.Contains(stdout, `Uploaded 2 jobs for "deploy-b"`) || pipelineUploads(runner) != 2 {
		t.Fatalf("continue deploy-b after a partial gate race code = %d, uploads = %d, stdout = %q, stderr = %q", code, pipelineUploads(runner), stdout, stderr)
	}
	var firstUpload []byte
	for _, command := range runner.commands {
		if slices.Equal(command.args, []string{"pipeline", "upload", "--no-interpolation"}) {
			firstUpload = command.stdin
			break
		}
	}
	first, _ := blockKeys(firstUpload)
	retried, referenced := blockKeys(lastPipelineUpload(t, runner))
	if len(first) != 2 || len(retried) != 1 || retried[productionGate] != "" || !referenced[productionGate] {
		t.Fatalf("first upload created gates %v, retry created %v and referenced %v; want the retry to reference %q and create only the staging gate:\n%s", first, retried, referenced, productionGate, lastPipelineUpload(t, runner))
	}
	for key, label := range retried {
		if !strings.Contains(label, "staging") {
			t.Fatalf("retry created %q (%q), want the staging gate", key, label)
		}
	}
}

// TestCheckContinuationsUnchanged pins the drift rule for the workflow's other
// continuations: the recompilation must defer exactly the recorded set.
func TestCheckContinuationsUnchanged(t *testing.T) {
	descriptor := func(job, producer string) compiler.RuntimeMatrixDescriptor {
		return compiler.RuntimeMatrixDescriptor{Schema: compiler.RuntimeMatrixSchemaV1, Job: job, Shape: compiler.RuntimeMatrixShapeInclude, ProducerJob: producer, ProducerStepKey: "gha-" + producer, ProducerOutput: "matrix"}
	}
	other := compiler.RuntimeMatrixContinuation{Descriptor: descriptor("build-b", "plan-b"), StepKey: "gha-build-b-matrix", ProducerStepKey: "gha-plan-b", Jobs: []string{"build-b", "publish-b"}, Labels: map[string]string{"build-b": "build-b", "publish-b": "publish-b"}}
	moved := other
	moved.Jobs = []string{"build-b"}
	extra := compiler.RuntimeMatrixContinuation{Descriptor: descriptor("build-c", "plan-c"), StepKey: "gha-build-c-matrix", ProducerStepKey: "gha-plan-c", Jobs: []string{"build-c"}, Labels: map[string]string{"build-c": "build-c"}}
	for _, test := range []struct {
		name                string
		recorded, remaining []compiler.RuntimeMatrixContinuation
		want                string
	}{
		{name: "single continuation", want: ""},
		{name: "other continuation still deferred", recorded: []compiler.RuntimeMatrixContinuation{other}, remaining: []compiler.RuntimeMatrixContinuation{other}, want: ""},
		{name: "unexpected continuation", remaining: []compiler.RuntimeMatrixContinuation{other}, want: `defers job "build-b" through step "gha-build-b-matrix", which the initial upload did not create`},
		{name: "continuation disappeared", recorded: []compiler.RuntimeMatrixContinuation{other}, want: `no longer defers job "build-b"`},
		{name: "continuation changed", recorded: []compiler.RuntimeMatrixContinuation{other}, remaining: []compiler.RuntimeMatrixContinuation{moved}, want: `deferred step "gha-build-b-matrix" compiled differently`},
		{name: "one of two disappeared", recorded: []compiler.RuntimeMatrixContinuation{other, extra}, remaining: []compiler.RuntimeMatrixContinuation{extra}, want: `no longer defers job "build-b"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkContinuationsUnchanged(test.recorded, test.remaining)
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestCheckDriftRequiresRecordedDeferredSources pins the provenance rule for
// deferred jobs: every expanded job must come from the workflow source the
// importer compiled, including the commit a remote reusable workflow was pinned
// to, because a deferred job has no earlier plan digest to compare.
func TestCheckDriftRequiresRecordedDeferredSources(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	remote := &compiler.RemoteWorkflowSource{Repository: "owner/workflows", RequestedRef: "v1", Commit: strings.Repeat("a", 40), SourceDigest: "sha256:" + strings.Repeat("2", 64)}
	run := continuationRun{artifact: continuationArtifact{
		Runtimes: map[string]string{compiler.PlatformLinuxAMD64.String(): digest},
		Continuation: compiler.RuntimeMatrixContinuation{
			Jobs: []string{"build", "call.publish"},
			Sources: map[string]compiler.RuntimeMatrixJobSource{
				"build":        {Path: "./.github/workflows/build.yml", Digest: digest},
				"call.publish": {Path: "owner/workflows/.github/workflows/publish.yml@v1", Digest: digest, Remote: remote},
			},
		},
	}}
	instance := func(job, path string, remote *compiler.RemoteWorkflowSource) compiler.JobInstance {
		return compiler.JobInstance{Key: "gha-" + strings.ReplaceAll(job, ".", "-"), LogicalJobID: job, Platform: compiler.PlatformLinuxAMD64, SourcePath: path, SourceDigest: digest, RemoteWorkflow: remote}
	}
	build := instance("build", "./.github/workflows/build.yml", nil)
	publish := instance("call.publish", "owner/workflows/.github/workflows/publish.yml@v1", remote)
	moved := *remote
	moved.Commit = strings.Repeat("b", 40)
	movedPublish := publish
	movedPublish.RemoteWorkflow = &moved
	editedBuild := build
	editedBuild.SourceDigest = "sha256:" + strings.Repeat("3", 64)
	unrecorded := instance("extra", "./.github/workflows/build.yml", nil)
	bundle := func(jobs ...compiler.JobInstance) compiler.Bundle {
		var b compiler.Bundle
		for _, job := range jobs {
			b.IR.Jobs = append(b.IR.Jobs, job)
			b.Plans = append(b.Plans, compiler.PlanArtifact{Job: plan.Job{Target: plan.Target{StepKey: job.Key}}, Digest: digest})
		}
		return b
	}
	for _, test := range []struct {
		name string
		jobs []compiler.JobInstance
		want string
	}{
		{name: "same sources", jobs: []compiler.JobInstance{build, publish}},
		{name: "remote tag moved", jobs: []compiler.JobInstance{build, movedPublish}, want: `deferred job "gha-call-publish" comes from a different workflow source`},
		{name: "workflow file edited", jobs: []compiler.JobInstance{editedBuild, publish}, want: `deferred job "gha-build" comes from a different workflow source`},
		{name: "job not in the initial upload", jobs: []compiler.JobInstance{build, publish, unrecorded}, want: `job "gha-extra" did not exist in the initial upload`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := run.checkDrift(bundle(test.jobs...))
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestContinueSkipsDeferredJobsWhenProducerFails proves a producer that did
// not succeed still resolves the deferred jobs and their GitHub checks, the
// way a skipped static job would.
func TestContinueSkipsDeferredJobsWhenProducerFails(t *testing.T) {
	initial := runContinueInitialUpload(t)
	prefix := strings.TrimSuffix(initial.artifact.Producer.StepKey, "plan")
	runner := initial.continueRunner(initial.producerManifest(t, "failure", ""))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 || !strings.Contains(stdout, `finished with result "failure"; the deferred jobs are skipped`) {
		t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(runner.uploaded) != 0 {
		t.Fatalf("skipped jobs uploaded artifacts: %v", slices.Sorted(maps.Keys(runner.uploaded)))
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	if len(steps) != 2 || steps[0].Key != prefix+"build" || steps[1].Key != prefix+"publish" {
		t.Fatalf("skipped steps = %#v", steps)
	}
	for _, step := range steps {
		if !strings.Contains(step.Skip, `matrix producer job "plan" finished with result failure`) || len(step.DependsOn) != 1 || step.DependsOn[0].Step != prefix+"plan" {
			t.Fatalf("skipped step = %#v", step)
		}
	}
}

// TestUploadFailsWholeWorkflowWhenStaticJobCannotJoinDeferredGraph proves a
// workflow that defers a matrix is never uploaded job by job. A per-job upload
// renders only the static jobs, so the continuation step and the deferred
// jobs would vanish and the build could pass without running them.
func TestUploadFailsWholeWorkflowWhenStaticJobCannotJoinDeferredGraph(t *testing.T) {
	requireImporterHost(t)
	for _, tolerated := range []bool{false, true} {
		t.Run(fmt.Sprintf("continue-on-error=%t", tolerated), func(t *testing.T) {
			lint := "    steps:\n      - uses: ./missing-action\n"
			if tolerated {
				lint = "    continue-on-error: true\n" + lint
			}
			source := strings.Replace(continueDeferredMatrixWorkflow, "    steps:\n      - run: true\n  plan:", lint+"  plan:", 1)
			if source == continueDeferredMatrixWorkflow {
				t.Fatal("workflow fixture did not change")
			}
			eventPath := pushEventPath(t)
			workflows := writeCommittedUploadWorkflows(t, map[string]string{"build.yml": source})
			t.Setenv("BUILDKITE", "true")
			t.Setenv("BUILDKITE_STEP_KEY", "continue-importer")
			t.Setenv("BUILDKITE_JOB_ID", continueImporterJobID)
			runner := &cliCaptureRunner{webhookErr: errors.New("metadata must not be read with --event-path")}
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"upload", "--event-path", eventPath}, workflows...), &stdout, &stderr, "dev", runner)
			if code != 0 {
				t.Fatalf("upload code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			var pipeline struct {
				Steps []struct {
					Group   string             `yaml:"group"`
					Label   string             `yaml:"label"`
					Command string             `yaml:"command"`
					Plugins failureStepPlugins `yaml:"plugins"`
					Steps   []map[string]any   `yaml:"steps"`
				} `yaml:"steps"`
			}
			if err := yaml.Unmarshal(lastPipelineUpload(t, runner), &pipeline); err != nil {
				t.Fatal(err)
			}
			if len(pipeline.Steps) != 1 || pipeline.Steps[0].Group != "" || len(pipeline.Steps[0].Steps) != 0 || pipeline.Steps[0].Label != ":github: workflow · .github/workflows/build.yml" || !isGeneratedFailureCommand(pipeline.Steps[0].Command) {
				t.Fatalf("workflow was not failed as a whole: %s", lastPipelineUpload(t, runner))
			}
			message := failureLogText(failureArtifactForStep(pipeline.Steps[0].Plugins, runner.uploaded, "messages"))
			for _, want := range []string{
				`Local action "./missing-action" is unavailable during compilation.`,
				`Error: jobs "build", "publish" are compiled and uploaded by step "gha-build-matrix" after job "plan" finishes, so this workflow cannot be uploaded job by job; every job fails until the workflow compiles as a whole {job=build}`,
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("failure message lacks %q: %q", want, message)
				}
			}
			for path := range runner.uploaded {
				if strings.HasPrefix(path, ".buildkite-gha/plans/") || strings.HasPrefix(path, ".buildkite-gha/continuations/") {
					t.Fatalf("failed workflow uploaded a runnable artifact %s: %v", path, slices.Sorted(maps.Keys(runner.uploaded)))
				}
			}
		})
	}
	t.Run("intact workflow still defers the matrix", func(t *testing.T) {
		initial := runContinueInitialUpload(t)
		if !strings.Contains(initial.pipeline, "gha-lint") || !strings.Contains(initial.pipeline, "gha-plan") || !strings.Contains(initial.pipeline, "--continuation-digest") {
			t.Fatalf("intact workflow pipeline = %s", initial.pipeline)
		}
	})
}

// TestContinueSkipsStaticMatrixInstancesWhenProducerFails proves a deferred
// dependent with a static matrix gets one skipped placeholder per instance,
// under the instance keys and check names its static expansion promised,
// rather than one placeholder under its logical key.
func TestContinueSkipsStaticMatrixInstancesWhenProducerFails(t *testing.T) {
	source := strings.Replace(continueDeferredMatrixWorkflow, "  publish:\n    needs: build\n    runs-on: ubuntu-latest\n", "  publish:\n    needs: build\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        os: [linux, macos]\n", 1)
	continuations := runContinueInitialUploads(t, source)
	initial := continuations[0]
	if len(initial.artifact.Continuation.Instances["publish"]) != 2 {
		t.Fatalf("continuation instances = %#v", initial.artifact.Continuation.Instances)
	}
	prefix := strings.TrimSuffix(initial.artifact.Producer.StepKey, "plan")
	runner := initial.continueRunner(initial.producerManifest(t, "failure", ""))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 {
		t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	if len(steps) != 3 || steps[0].Key != prefix+"build" {
		t.Fatalf("skipped steps = %#v, want build and both publish instances", steps)
	}
	checks := map[string]string{}
	for _, step := range steps[1:] {
		if !strings.HasPrefix(step.Key, prefix+"publish-") || step.Key == prefix+"publish" || !strings.Contains(step.Skip, "finished with result failure") {
			t.Fatalf("skipped step = %#v, want a publish instance", step)
		}
		checks[step.Key] = step.Notify[0].GitHubCheck.Name
	}
	for _, instance := range initial.artifact.Continuation.Instances["publish"] {
		if !strings.Contains(checks[instance.Key], instance.CheckLabel) {
			t.Fatalf("checks = %v, want %q for %q", checks, instance.CheckLabel, instance.Key)
		}
	}
	names := strings.Join(slices.Sorted(maps.Values(checks)), "\n")
	if !strings.Contains(names, `publish (os="linux")`) || !strings.Contains(names, `publish (os="macos")`) {
		t.Fatalf("check names = %q, want one per matrix value", names)
	}
}

// TestContinueReplayIsIdempotent exercises the retry behavior Buildkite gives
// a dynamic upload: a replayed upload whose step keys already exist is
// rejected as a whole. The continuation treats that rejection as success only
// when every deferred step already exists with the plan this run compiled;
// any other rejection fails with whole-build retry guidance.
func TestContinueReplayIsIdempotent(t *testing.T) {
	initial := runContinueInitialUpload(t)
	matrix := `[{"target":"x","runner":"ubuntu-latest"}]`
	first := initial.continueRunner(initial.producerManifest(t, "success", matrix))
	if code, stdout, stderr := runContinue(t, first, initial.digest); code != 0 {
		t.Fatalf("first continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, first))
	existing := make(map[string]map[string]string, len(steps))
	for _, step := range steps {
		existing[step.Key] = map[string]string{"command": step.Command, "label": step.Label}
	}

	t.Run("replay after a successful upload", func(t *testing.T) {
		replay := initial.continueRunner(initial.producerManifest(t, "success", matrix))
		replay.pipelineUploadErr = errors.New(`pipeline upload: duplicate step key "` + steps[0].Key + `"`)
		replay.stepAttributes = existing
		code, stdout, stderr := runContinue(t, replay, initial.digest)
		if code != 0 || !strings.Contains(stdout, "were already uploaded by an earlier run of this step") {
			t.Fatalf("replay code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})

	t.Run("rejected upload with a missing step", func(t *testing.T) {
		partial := initial.continueRunner(initial.producerManifest(t, "success", matrix))
		partial.pipelineUploadErr = errors.New("pipeline upload: rejected")
		partial.stepAttributes = map[string]map[string]string{steps[0].Key: existing[steps[0].Key]}
		code, _, stderr := runContinue(t, partial, initial.digest)
		if code != 1 || !strings.Contains(stderr, "pipeline upload: rejected") || !strings.Contains(stderr, continueRetryGuidance) {
			t.Fatalf("partial code = %d, stderr = %q", code, stderr)
		}
	})

	t.Run("rejected upload with a different plan", func(t *testing.T) {
		changed := initial.continueRunner(initial.producerManifest(t, "success", `[{"target":"y","runner":"ubuntu-latest"}]`))
		changed.pipelineUploadErr = errors.New("pipeline upload: rejected")
		changed.stepAttributes = existing
		code, _, stderr := runContinue(t, changed, initial.digest)
		if code != 1 || !strings.Contains(stderr, continueRetryGuidance) {
			t.Fatalf("changed code = %d, stderr = %q", code, stderr)
		}
	})

	t.Run("skip replay", func(t *testing.T) {
		skipped := initial.continueRunner(initial.producerManifest(t, "failure", ""))
		if code, stdout, stderr := runContinue(t, skipped, initial.digest); code != 0 {
			t.Fatalf("skip code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		_, _, skipSteps := decodeContinuePipeline(t, lastPipelineUpload(t, skipped))
		replay := initial.continueRunner(initial.producerManifest(t, "failure", ""))
		replay.pipelineUploadErr = errors.New("pipeline upload: rejected")
		replay.stepAttributes = map[string]map[string]string{}
		for _, step := range skipSteps {
			replay.stepAttributes[step.Key] = map[string]string{"label": step.Label}
		}
		code, stdout, stderr := runContinue(t, replay, initial.digest)
		if code != 0 || !strings.Contains(stdout, "already uploaded") {
			t.Fatalf("skip replay code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})
}

// TestContinuationArtifactRoundTrip proves the artifact decodes strictly and
// records the values the continuation needs.
func TestContinuationArtifactRoundTrip(t *testing.T) {
	initial := runContinueInitialUpload(t, "--runner-queue", "ubuntu-latest=custom-linux")
	artifact := initial.artifact
	if artifact.Schema != continuationSchema || artifact.Importer != continueImporterJobID || artifact.Workflow.Path != ".github/workflows/build.yml" {
		t.Fatalf("artifact = %+v", artifact)
	}
	// --event-path supplies a payload file, which every plan digest records.
	if artifact.Event.Name != "push" || !artifact.Event.File {
		t.Fatalf("artifact event = %+v", artifact.Event)
	}
	if len(artifact.Runners) != 1 || artifact.Runners[0].Label != "ubuntu-latest" || artifact.Runners[0].Queue != "custom-linux" {
		t.Fatalf("artifact runners = %+v", artifact.Runners)
	}
	if len(artifact.Graph) != 2 || artifact.Graph[0].Job != "lint" || artifact.Graph[1].Job != "plan" {
		t.Fatalf("artifact graph = %+v", artifact.Graph)
	}
	// Every deferred job records the workflow source it must be compiled from.
	sources := artifact.Continuation.Sources
	if len(sources) != 2 || sources["build"].Path != "./.github/workflows/build.yml" || sources["build"].Remote != nil || sources["publish"] != sources["build"] {
		t.Fatalf("artifact sources = %+v", sources)
	}
	var loose map[string]any
	if err := json.Unmarshal(initial.data, &loose); err != nil {
		t.Fatal(err)
	}
	loose["extra"] = true
	extra, _ := json.Marshal(loose)
	if _, err := decodeContinuationArtifact(extra, "dev"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	delete(loose, "extra")
	delete(loose["continuation"].(map[string]any), "sources")
	withoutSources, _ := json.Marshal(loose)
	if _, err := decodeContinuationArtifact(withoutSources, "dev"); err == nil || !strings.Contains(err.Error(), "must record the source of every deferred job") {
		t.Fatalf("missing sources error = %v", err)
	}
	// lint and plan are static, so the one deferred upload gets the rest of
	// the graph bound; the budget must hold the promised jobs and stay within
	// the bound.
	if artifact.Continuation.JobBudget != compiler.MaxRuntimeMatrixGraphJobs-2 {
		t.Fatalf("artifact job budget = %d, want %d", artifact.Continuation.JobBudget, compiler.MaxRuntimeMatrixGraphJobs-2)
	}
	for _, budget := range []int{0, 1, compiler.MaxRuntimeMatrixGraphJobs + 1} {
		withBudget := bytes.Replace(initial.data, []byte(fmt.Sprintf(`"job_budget":%d`, artifact.Continuation.JobBudget)), []byte(fmt.Sprintf(`"job_budget":%d`, budget)), 1)
		if _, err := decodeContinuationArtifact(withBudget, "dev"); err == nil || !strings.Contains(err.Error(), "records an invalid job budget") {
			t.Fatalf("job budget %d error = %v", budget, err)
		}
	}
	if _, err := decodeContinuationArtifact(initial.data, "other"); err == nil || !strings.Contains(err.Error(), "written by buildkite-gha dev") {
		t.Fatalf("version error = %v", err)
	}
	// The workflow path must stay inside the checkout; ".." within a filename
	// is a valid name, a ".." segment or an absolute path is not.
	withPath := func(workflowPath string) []byte {
		return bytes.Replace(initial.data, []byte(`".github/workflows/build.yml"`), []byte(`"`+workflowPath+`"`), 1)
	}
	for _, valid := range []string{".github/workflows/ci..matrix.yml", ".github/workflows/..build.yml"} {
		if _, err := decodeContinuationArtifact(withPath(valid), "dev"); err != nil {
			t.Fatalf("path %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"../build.yml", ".github/../../build.yml", "/etc/build.yml", ".github//workflows/build.yml", ""} {
		if _, err := decodeContinuationArtifact(withPath(invalid), "dev"); err == nil || !strings.Contains(err.Error(), "invalid workflow path") {
			t.Fatalf("path %q error = %v", invalid, err)
		}
	}
}

// TestContinueReadsPrivateReusableWorkflowsLikeTheImporter proves that the
// plugin's private-reusable-workflows setting travels with the continuation:
// the deferred step reads remote reusable workflows through the same Git
// source the importer used, and it fails before uploading anything when that
// source cannot be built.
func TestContinueReadsPrivateReusableWorkflowsLikeTheImporter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to configure the private repository source")
	}
	// Both uploads use the same event file; the first upload changes into its
	// own checkout, so resolve the path before either runs.
	eventPath := pushEventPath(t)
	uploadFor := func(uploadArgs ...string) continueInitialUpload {
		continuations := runContinueInitialUploadsForEvent(t, continueDeferredMatrixWorkflow, eventPath, uploadArgs...)
		if len(continuations) != 1 {
			t.Fatalf("upload wrote %d continuations, want 1", len(continuations))
		}
		return continuations[0]
	}
	initial := uploadFor("--private-reusable-workflows")
	if !initial.artifact.PrivateReusableWorkflows {
		t.Fatalf("artifact does not record the private reusable workflows setting: %+v", initial.artifact)
	}
	if !strings.Contains(string(initial.data), `"private_reusable_workflows":true`) {
		t.Fatalf("artifact JSON omits the setting:\n%s", initial.data)
	}
	withoutSetting := uploadFor()
	if withoutSetting.artifact.PrivateReusableWorkflows || strings.Contains(string(withoutSetting.data), "private_reusable_workflows") {
		t.Fatalf("artifact records a setting the importer did not use:\n%s", withoutSetting.data)
	}
	matrix := `[{"target":"amd64","runner":"ubuntu-latest"}]`

	t.Run("git_available", func(t *testing.T) {
		runner := initial.continueRunner(initial.producerManifest(t, "success", matrix))
		code, stdout, stderr := runContinue(t, runner, initial.digest)
		if code != 0 {
			t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if pipelineUploads(runner) != 1 || !strings.Contains(stdout, "Uploaded 2 jobs for \"build\"") {
			t.Fatalf("pipeline uploads = %d, stdout = %q", pipelineUploads(runner), stdout)
		}
	})

	t.Run("git_missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		runner := initial.continueRunner(initial.producerManifest(t, "success", matrix))
		code, stdout, stderr := runContinue(t, runner, initial.digest)
		if code != 1 || !strings.Contains(stderr, "resolve Git executable") || !strings.Contains(stderr, "repository source could not be configured") {
			t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if pipelineUploads(runner) != 0 {
			t.Fatalf("continue uploaded a pipeline without its repository source: %#v", runner.commands)
		}
		// The importer without the setting does not need Git at all.
		runner = withoutSetting.continueRunner(withoutSetting.producerManifest(t, "success", matrix))
		if code, stdout, stderr := runContinue(t, runner, withoutSetting.digest); code != 0 {
			t.Fatalf("continue without the setting code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})
}

// TestContinueSharesTheLargestEventUploadAdmits proves that the event source
// is uploaded once per upload rather than copied into every continuation: with
// the largest event upload accepts and two needs-derived matrices, the upload
// writes one event artifact of that size, every continuation stays small, and
// each continuation still reads the event it was compiled from.
func TestContinueSharesTheLargestEventUploadAdmits(t *testing.T) {
	var event map[string]any
	source, err := os.ReadFile(pushEventPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(source, &event); err != nil {
		t.Fatal(err)
	}
	// Leave room for the fields around the payload so the whole event stays
	// just inside plan.MaxEventPayloadBytes.
	event["payload"] = map[string]string{"ref": "refs/heads/main", "body": strings.Repeat("x", plan.MaxEventPayloadBytes-4096)}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > plan.MaxEventPayloadBytes {
		t.Fatalf("event is %d bytes, over the %d-byte limit", len(encoded), plan.MaxEventPayloadBytes)
	}
	eventPath := filepath.Join(t.TempDir(), "push.json")
	if err := os.WriteFile(eventPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	continuations := runContinueInitialUploadsForEvent(t, continueTwoMatricesWorkflow, eventPath)
	if len(continuations) != 2 {
		t.Fatalf("upload wrote %d continuations, want 2", len(continuations))
	}
	const smallContinuation = 64 * 1024
	for _, initial := range continuations {
		if len(initial.eventData) <= plan.MaxEventPayloadBytes/2 || len(initial.data) > smallContinuation {
			t.Fatalf("event artifact is %d bytes and continuation %s is %d bytes; want the event near %d and the continuation under %d", len(initial.eventData), initial.digest, len(initial.data), plan.MaxEventPayloadBytes, smallContinuation)
		}
	}

	initial := continuations[0]
	runner := initial.continueRunner(initial.producerManifest(t, "success", `[{"target":"a1"}]`))
	code, stdout, stderr := runContinue(t, runner, initial.digest)
	if code != 0 || !strings.Contains(stdout, "Uploaded 1 jobs for \"build-a\"") {
		t.Fatalf("continue code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	// A tampered or missing event fails closed: the digest recorded in the
	// continuation must match what the continuation downloads.
	tampered := initial.continueRunner(initial.producerManifest(t, "success", `[{"target":"a1"}]`))
	tampered.dataByPath[initial.eventPath] = bytes.Replace(initial.eventData, []byte("refs/heads/main"), []byte("refs/heads/evil"), 1)
	if code, _, stderr := runContinue(t, tampered, initial.digest); code != 1 || !strings.Contains(stderr, "event digest") {
		t.Fatalf("tampered event code = %d, stderr = %q", code, stderr)
	}
}

// TestCompileReportsDeferredMatrixWorkflows proves that compile keeps a
// deferred-matrix workflow compilable: the IR carries the continuation, and
// the pipeline format explains why it cannot render the deferred step instead
// of reporting the workflow as incompatible.
func TestCompileReportsDeferredMatrixWorkflows(t *testing.T) {
	requireImporterHost(t)
	eventPath := pushEventPath(t)
	workflows := writeCommittedUploadWorkflows(t, map[string]string{"build.yml": continueDeferredMatrixWorkflow})
	runner := &cliCaptureRunner{}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"compile", "--event-path", eventPath, "--format", "ir-json", workflows[0]}, &stdout, &stderr, "dev", runner); code != 0 {
		t.Fatalf("compile ir-json code = %d, stderr = %q", code, stderr.String())
	}
	var ir compiler.IR
	if err := json.Unmarshal(stdout.Bytes(), &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Continuations) != 1 || ir.Continuations[0].StepKey != "gha-build-matrix" || len(ir.Jobs) != 2 || !strings.Contains(stderr.String(), "[W_MATRIX_DEFERRED]") {
		t.Fatalf("ir continuations = %#v jobs = %d stderr = %q", ir.Continuations, len(ir.Jobs), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"compile", "--event-path", eventPath, workflows[0]}, &stdout, &stderr, "dev", runner); code != 1 {
		t.Fatalf("compile pipeline code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Result: compilable") || !strings.Contains(stderr.String(), `job "build" takes its matrix from a job output`) || !strings.Contains(stderr.String(), "Use --format ir-json") {
		t.Fatalf("compile pipeline stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
