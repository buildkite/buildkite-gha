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
	artifact     stageRecord
	eventPath    string
	eventData    []byte
	pipeline     string
	plans        map[string][]byte
}

// rootProducer returns the graph entry of the first root's producer.
func (s stageRecord) rootProducer() compiledJob {
	return s.producer(s.Continuation.Descriptor.Job)
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
		case strings.HasPrefix(path, ".buildkite-gha/stages/events/"):
			events = append(events, path)
			if got := strings.TrimPrefix(transport.Digest(contents), "sha256:"); !strings.HasSuffix(path, "/"+got+".json") {
				t.Fatalf("event artifact %s is not addressed by its digest %s", path, got)
			}
		case strings.HasPrefix(path, ".buildkite-gha/stages/"):
			artifact, err := decodeStageRecord(contents, "dev")
			if err != nil {
				t.Fatal(err)
			}
			digest := transport.Digest(contents)
			if !strings.Contains(pipeline, "--stage-digest '"+digest+"'") {
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
		eventPath, err := buildkitepipeline.StageEventPath(continuations[i].artifact.Event.Digest)
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
		PlanDigest: initial.artifact.rootProducer().PlanDigest,
		Producer:   transport.Producer{BuildID: continueBuildID, JobID: jobID, StepKey: initial.artifact.rootProducer().Key},
		Result:     result,
	}
	if matrix != "" {
		manifest.Outputs = []transport.Output{{Name: initial.artifact.Continuation.Descriptor.ProducerOutput, Value: matrix}}
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
		jobByStep:  map[string]string{initial.artifact.rootProducer().Key: continueProducerJobID},
		dataByPath: initial.dataByPath(manifest),
	}
}

// dataByPath is artifactData plus the producer's published result.
func (initial continueInitialUpload) dataByPath(manifest []byte) map[string][]byte {
	data := initial.artifactData()
	data[transport.ResultPath(initial.artifact.rootProducer().Key, initial.artifact.rootProducer().PlanDigest)] = manifest
	return data
}

func runContinue(t *testing.T, runner *cliCaptureRunner, digest string) (int, string, string) {
	t.Helper()
	return runContinueAs(t, runner, digest, continueImporterJobID, continueContinuationJobID)
}

// runContinueAs runs the deferred step as job jobID, reading the continuation
// the job producer wrote: the importer for the first stage of a component,
// the earlier stage's step for every later one.
func runContinueAs(t *testing.T, runner *cliCaptureRunner, digest, producer, jobID string) (int, string, string) {
	t.Helper()
	t.Setenv("BUILDKITE", "true")
	t.Setenv("BUILDKITE_BUILD_ID", continueBuildID)
	t.Setenv("BUILDKITE_JOB_ID", jobID)
	var stdout, stderr bytes.Buffer
	code := run([]string{"upload", "--stage-digest", digest, "--stage-producer", producer}, &stdout, &stderr, "dev", runner)
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
	prefix := strings.TrimSuffix(initial.artifact.rootProducer().Key, "plan")
	if initial.artifact.rootProducer().Key != prefix+"plan" || !slices.Equal(initial.artifact.Continuation.Jobs, []string{"build", "publish"}) || initial.artifact.Continuation.StepKey != prefix+"build-matrix" {
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
			if code != 1 || !strings.Contains(stderr, test.want) || !strings.Contains(stderr, stageRetryGuidance) {
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
// continuation its step names by digest and only the workflow it compiled.
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
		code := run([]string{"upload", "--stage-digest", initial.digest, "--stage-producer", continueImporterJobID}, &stdout, &stderr, "dev", runner)
		if code != 2 || !strings.Contains(stderr.String(), "BUILDKITE_JOB_ID") || len(runner.commands) != 0 {
			t.Fatalf("continue code = %d, stderr = %q, commands = %#v", code, stderr.String(), runner.commands)
		}
	})
	// The stage form takes only the record and its producer; the importer's
	// options and workflow operands are recorded inputs it must not override,
	// and the command the earlier releases emitted no longer exists.
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "workflow operand", args: []string{"upload", "--stage-digest", initial.digest, "--stage-producer", continueImporterJobID, "build.yml"}, want: `"build.yml" cannot be combined with --stage-digest`},
		{name: "importer option", args: []string{"upload", "--stage-digest", initial.digest, "--stage-producer", continueImporterJobID, "--runner-queue", "ubuntu-latest=q"}, want: `"--runner-queue" cannot be combined with --stage-digest`},
		{name: "missing producer", args: []string{"upload", "--stage-digest", initial.digest}, want: "--stage-producer requires"},
		{name: "invalid digest", args: []string{"upload", "--stage-digest", "sha256:nope", "--stage-producer", continueImporterJobID}, want: "--stage-digest requires a sha256 digest"},
		{name: "continue command removed", args: []string{"continue", "--continuation-digest", initial.digest, "--continuation-producer", continueImporterJobID}, want: `unknown command "continue"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := initial.continueRunner(manifest)
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr, "dev", runner); code != 2 || !strings.Contains(stderr.String(), test.want) || len(runner.commands) != 0 {
				t.Fatalf("code = %d, stderr = %q, want %q; commands = %#v", code, stderr.String(), test.want, runner.commands)
			}
		})
	}
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
	producerKey := initial.artifact.rootProducer().Key
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
	for _, key := range []string{a.artifact.Continuation.StepKey, b.artifact.Continuation.StepKey, a.artifact.rootProducer().Key, b.artifact.rootProducer().Key} {
		if !strings.Contains(a.pipeline, `key: "`+key+`"`) {
			t.Fatalf("initial upload is missing step %q:\n%s", key, a.pipeline)
		}
	}

	const producerBJobID = "0192f7d0-0000-4000-8000-00000000bbb2"
	runner := &cliCaptureRunner{
		jobByStep: map[string]string{a.artifact.rootProducer().Key: continueProducerJobID, b.artifact.rootProducer().Key: producerBJobID},
		dataByPath: map[string][]byte{
			a.artifactPath: a.data,
			b.artifactPath: b.data,
			a.eventPath:    a.eventData,
			transport.ResultPath(a.artifact.rootProducer().Key, a.artifact.rootProducer().PlanDigest): a.producerManifest(t, "success", `[{"target":"a1"},{"target":"a2"}]`),
			transport.ResultPath(b.artifact.rootProducer().Key, b.artifact.rootProducer().PlanDigest): b.producerManifestFrom(t, producerBJobID, "success", `[{"target":"b1"}]`),
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
			jobByStep: map[string]string{a.artifact.rootProducer().Key: continueProducerJobID, b.artifact.rootProducer().Key: producerBJobID},
			dataByPath: map[string][]byte{
				a.artifactPath: a.data,
				b.artifactPath: b.data,
				a.eventPath:    a.eventData,
				transport.ResultPath(a.artifact.rootProducer().Key, a.artifact.rootProducer().PlanDigest): a.producerManifest(t, "success", `[{"target":"a1"}]`),
				transport.ResultPath(b.artifact.rootProducer().Key, b.artifact.rootProducer().PlanDigest): b.producerManifestFrom(t, producerBJobID, "success", `[{"target":"b1"}]`),
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
		jobByStep: map[string]string{a.artifact.rootProducer().Key: continueProducerJobID, b.artifact.rootProducer().Key: producerBJobID},
		dataByPath: map[string][]byte{
			a.artifactPath: a.data,
			b.artifactPath: b.data,
			a.eventPath:    a.eventData,
			transport.ResultPath(b.artifact.rootProducer().Key, b.artifact.rootProducer().PlanDigest): b.producerManifestFrom(t, producerBJobID, "success", `[{"target":"b1"}]`),
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

// TestAdvanceRequiresOtherContinuationsUnchanged pins the drift rule for the
// workflow's other continuations: the recompilation must defer exactly the
// recorded set.
func TestAdvanceRequiresOtherContinuationsUnchanged(t *testing.T) {
	descriptor := func(job, producer string) compiler.RuntimeOutputDescriptor {
		return compiler.RuntimeOutputDescriptor{Schema: compiler.RuntimeMatrixSchemaV1, Job: job, Shape: compiler.RuntimeMatrixShapeInclude, ProducerJob: producer, ProducerStepKey: "gha-" + producer, ProducerOutput: "matrix"}
	}
	other := compiler.RuntimeContinuation{Descriptor: descriptor("build-b", "plan-b"), StepKey: "gha-build-b-matrix", ProducerStepKey: "gha-plan-b", Jobs: []string{"build-b", "publish-b"}, Labels: map[string]string{"build-b": "build-b", "publish-b": "publish-b"}}
	moved := other
	moved.Jobs = []string{"build-b"}
	extra := compiler.RuntimeContinuation{Descriptor: descriptor("build-c", "plan-c"), StepKey: "gha-build-c-matrix", ProducerStepKey: "gha-plan-c", Jobs: []string{"build-c"}, Labels: map[string]string{"build-c": "build-c"}}
	for _, test := range []struct {
		name                string
		recorded, remaining []compiler.RuntimeContinuation
		want                string
	}{
		{name: "single continuation", want: ""},
		{name: "other continuation still deferred", recorded: []compiler.RuntimeContinuation{other}, remaining: []compiler.RuntimeContinuation{other}, want: ""},
		{name: "unexpected continuation", remaining: []compiler.RuntimeContinuation{other}, want: `defers job "build-b" through step "gha-build-b-matrix", which the initial upload did not create`},
		{name: "continuation disappeared", recorded: []compiler.RuntimeContinuation{other}, want: `no longer defers job "build-b"`},
		{name: "continuation changed", recorded: []compiler.RuntimeContinuation{other}, remaining: []compiler.RuntimeContinuation{moved}, want: `deferred step "gha-build-b-matrix" compiled differently`},
		{name: "one of two disappeared", recorded: []compiler.RuntimeContinuation{other, extra}, remaining: []compiler.RuntimeContinuation{extra}, want: `no longer defers job "build-b"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := stageRecord{Continuation: compiler.RuntimeContinuation{StepKey: "gha-build-a-matrix"}, Others: test.recorded}
			var bundle compiler.Bundle
			bundle.IR.Continuations = test.remaining
			_, err := record.advance(bundle, nil, nil, "")
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestAdvanceRequiresRecordedDeferredSources pins the provenance rule for
// deferred jobs: every expanded job must come from the workflow source the
// importer compiled, including the commit a remote reusable workflow was pinned
// to, because a deferred job has no earlier plan digest to compare.
func TestAdvanceRequiresRecordedDeferredSources(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	remote := &compiler.RemoteWorkflowSource{Repository: "owner/workflows", RequestedRef: "v1", Commit: strings.Repeat("a", 40), SourceDigest: "sha256:" + strings.Repeat("2", 64)}
	record := stageRecord{
		Runtimes: map[string]string{compiler.PlatformLinuxAMD64.String(): digest},
		Continuation: compiler.RuntimeContinuation{
			StepKey:   "gha-build-matrix",
			JobBudget: 3,
			Jobs:      []string{"build", "call.publish"},
			Sources: map[string]compiler.RuntimeMatrixJobSource{
				"build":        {Path: "./.github/workflows/build.yml", Digest: digest},
				"call.publish": {Path: "owner/workflows/.github/workflows/publish.yml@v1", Digest: digest, Remote: remote},
			},
		},
	}
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
			b.GeneratedWorkflow.Jobs = append(b.GeneratedWorkflow.Jobs, buildkitepipeline.Job{Key: job.Key})
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
			_, err := record.advance(bundle(test.jobs...), nil, nil, "")
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
	prefix := strings.TrimSuffix(initial.artifact.rootProducer().Key, "plan")
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
				if strings.HasPrefix(path, ".buildkite-gha/plans/") || strings.HasPrefix(path, ".buildkite-gha/stages/") {
					t.Fatalf("failed workflow uploaded a runnable artifact %s: %v", path, slices.Sorted(maps.Keys(runner.uploaded)))
				}
			}
		})
	}
	t.Run("intact workflow still defers the matrix", func(t *testing.T) {
		initial := runContinueInitialUpload(t)
		if !strings.Contains(initial.pipeline, "gha-lint") || !strings.Contains(initial.pipeline, "gha-plan") || !strings.Contains(initial.pipeline, "--stage-digest") {
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
	prefix := strings.TrimSuffix(initial.artifact.rootProducer().Key, "plan")
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
		if code != 1 || !strings.Contains(stderr, "pipeline upload: rejected") || !strings.Contains(stderr, stageRetryGuidance) {
			t.Fatalf("partial code = %d, stderr = %q", code, stderr)
		}
	})

	t.Run("rejected upload with a different plan", func(t *testing.T) {
		changed := initial.continueRunner(initial.producerManifest(t, "success", `[{"target":"y","runner":"ubuntu-latest"}]`))
		changed.pipelineUploadErr = errors.New("pipeline upload: rejected")
		changed.stepAttributes = existing
		code, _, stderr := runContinue(t, changed, initial.digest)
		if code != 1 || !strings.Contains(stderr, stageRetryGuidance) {
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

const continueJoinedWorkflow = continueDeferredMatrixWorkflow + `  plan_other:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: [{one: true}]
    outputs:
      matrix: ${{ steps.rows.outputs.matrix }}
    steps:
      - id: rows
        run: true
  test:
    needs: plan_other
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.plan_other.outputs.matrix) }}
    steps:
      - run: true
  test_tail:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - run: true
  join:
    needs: [publish, test_tail, lint]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: true
`

func joinedContinueRunner(t *testing.T, initial continueInitialUpload, firstResult, secondResult string) *cliCaptureRunner {
	t.Helper()
	matrix := ""
	if firstResult == "success" {
		matrix = `[{"target":"x","runner":"ubuntu-latest"},{"target":"y","runner":"ubuntu-latest"}]`
	}
	runner := initial.continueRunner(initial.producerManifest(t, firstResult, matrix))
	producer := initial.artifact.producer("test")
	const jobID = "0192f7d0-0000-4000-8000-00000000bbb2"
	manifest := transport.ResultManifest{PlanDigest: producer.PlanDigest, Producer: transport.Producer{BuildID: continueBuildID, JobID: jobID, StepKey: producer.Key}, Result: secondResult}
	if secondResult == "success" {
		manifest.Outputs = []transport.Output{{Name: "matrix", Value: `[{"target":"z"}]`}}
	}
	data, err := transport.MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	runner.jobByStep[producer.Key] = jobID
	runner.dataByPath[transport.ResultPath(producer.Key, producer.PlanDigest)] = data
	return runner
}

func TestContinueJoinedClosures(t *testing.T) {
	initials := runContinueInitialUploads(t, continueJoinedWorkflow)
	if len(initials) != 1 {
		t.Fatalf("continuations = %d, want one owner", len(initials))
	}
	initial := initials[0]
	prefix := strings.TrimSuffix(initial.artifact.rootProducer().Key, "plan")
	producer := initial.artifact.producer("test")
	if producer.Key == prefix+"plan_other" || !strings.HasPrefix(producer.Key, prefix+"plan_other-") {
		t.Fatalf("producer lost its matrix instance key: %#v", producer)
	}
	_, _, initialSteps := decodeContinuePipeline(t, []byte(initial.pipeline))
	for _, step := range initialSteps {
		if step.Key == initial.artifact.Continuation.StepKey {
			if len(step.DependsOn) != 2 || step.DependsOn[0].Step != initial.artifact.rootProducer().Key || step.DependsOn[1].Step != producer.Key {
				t.Fatalf("owner dependencies = %#v", step.DependsOn)
			}
		}
	}
	for _, test := range []struct {
		first, second string
		runnable      int
		skipped       []string
	}{
		{first: "success", second: "success", runnable: 6},
		{first: "failure", second: "success", runnable: 2, skipped: []string{"build", "join", "publish"}},
		{first: "success", second: "cancelled", runnable: 3, skipped: []string{"join", "test", "test_tail"}},
		{first: "skipped", second: "failure", runnable: 0, skipped: []string{"build", "join", "publish", "test", "test_tail"}},
	} {
		t.Run(test.first+"/"+test.second, func(t *testing.T) {
			runner := joinedContinueRunner(t, initial, test.first, test.second)
			if code, stdout, stderr := runContinue(t, runner, initial.digest); code != 0 {
				t.Fatalf("continue = %d\n%s\n%s", code, stdout, stderr)
			}
			_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
			seen := make(map[string]bool)
			var skipped []string
			var runnable int
			existing := make(map[string]map[string]string)
			for _, step := range steps {
				if seen[step.Key] {
					t.Fatalf("duplicate upload of %s", step.Key)
				}
				seen[step.Key] = true
				if step.Skip != "" {
					skipped = append(skipped, strings.TrimPrefix(step.Key, prefix))
					if step.Command != "exit 1 # skipped by stage '"+initial.digest+"'" {
						t.Fatalf("skip is not bound to owner artifact: %q", step.Command)
					}
				} else {
					runnable++
				}
				if step.Key == prefix+"join" && step.Skip == "" {
					var dependencies []string
					for _, dependency := range step.DependsOn {
						dependencies = append(dependencies, dependency.Step)
					}
					slices.Sort(dependencies)
					if !slices.Equal(dependencies, []string{prefix + "lint", prefix + "publish", prefix + "test_tail"}) {
						t.Fatalf("join dependencies = %v", dependencies)
					}
				}
				existing[step.Key] = map[string]string{"command": step.Command}
			}
			slices.Sort(skipped)
			if runnable != test.runnable || !reflect.DeepEqual(skipped, test.skipped) || !seen[prefix+"join"] {
				t.Fatalf("runnable=%d skipped=%v steps=%#v", runnable, skipped, steps)
			}
			if seen[initial.artifact.rootProducer().Key] || seen[producer.Key] || seen[prefix+"lint"] {
				t.Fatal("uploaded a parent-owned prerequisite")
			}
			plans := uploadedPlans(t, runner)
			for _, job := range test.skipped {
				if len(plans[job]) != 0 {
					t.Fatalf("skipped %s has executable plans", job)
				}
			}
			replay := joinedContinueRunner(t, initial, test.first, test.second)
			replay.pipelineUploadErr = errors.New("duplicate step key")
			replay.stepAttributes = existing
			if code, _, stderr := runContinue(t, replay, initial.digest); code != 0 {
				t.Fatalf("replay = %d: %s", code, stderr)
			}
			join := existing[prefix+"join"]
			existing[prefix+"join"] = map[string]string{"command": "different plan or owner"}
			if code, _, _ := runContinue(t, replay, initial.digest); code != 1 {
				t.Fatalf("different join binding accepted as replay: %d", code)
			}
			existing[prefix+"join"] = join
			delete(existing, prefix+"join")
			if code, _, _ := runContinue(t, replay, initial.digest); code != 1 {
				t.Fatalf("missing join accepted as replay: %d", code)
			}
		})
	}
	t.Run("missing manifest is not a skip", func(t *testing.T) {
		runner := joinedContinueRunner(t, initial, "failure", "success")
		delete(runner.dataByPath, transport.ResultPath(producer.Key, producer.PlanDigest))
		if code, _, stderr := runContinue(t, runner, initial.digest); code != 1 || !strings.Contains(stderr, "result is unavailable") {
			t.Fatalf("code=%d: %s", code, stderr)
		}
		if pipelineUploads(runner) != 0 || len(runner.uploaded) != 0 {
			t.Fatal("uploaded jobs without every manifest")
		}
	})
	t.Run("second producer retried", func(t *testing.T) {
		runner := joinedContinueRunner(t, initial, "success", "success")
		runner.jobByStep[producer.Key] = "0192f7d0-0000-4000-8000-00000000bbb3"
		if code, _, stderr := runContinue(t, runner, initial.digest); code != 1 || !strings.Contains(stderr, "result is unavailable") || pipelineUploads(runner) != 0 {
			t.Fatalf("retried producer code=%d: %s", code, stderr)
		}
	})
	for _, test := range []struct {
		name string
		edit func(*stageRecord)
	}{
		{name: "producer without plan", edit: func(a *stageRecord) {
			for i := range a.Graph {
				if a.Graph[i].Key == producer.Key {
					a.Graph[i].PlanDigest = ""
				}
			}
		}},
		{name: "logical key instead of instance", edit: func(a *stageRecord) { a.Continuation.Joined[0].ProducerStepKey = prefix + "plan_other" }},
		{name: "producer of another job", edit: func(a *stageRecord) { a.Continuation.Joined[0].ProducerStepKey = prefix + "lint" }},
		{name: "missing producer", edit: func(a *stageRecord) {
			a.Graph = slices.DeleteFunc(a.Graph, func(job compiledJob) bool { return job.Key == producer.Key })
		}},
		{name: "duplicate root", edit: func(a *stageRecord) {
			a.Continuation.Joined = append(a.Continuation.Joined, a.Continuation.Joined[0])
		}},
		{name: "outside prerequisite claimed", edit: func(a *stageRecord) { a.Continuation.Joined[0].Descriptor.ProducerJob = "lint" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, err := decodeStageRecord(initial.data, "dev")
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&a)
			data, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeStageRecord(data, "dev"); err == nil {
				t.Fatal("accepted incorrect producer binding")
			}
		})
	}
	for _, budget := range []int{5, 6} {
		t.Run(fmt.Sprintf("component budget %d", budget), func(t *testing.T) {
			limited := initial
			limited.artifact.Continuation.JobBudget = budget
			data, err := json.Marshal(limited.artifact)
			if err != nil {
				t.Fatal(err)
			}
			limited.data, limited.digest = data, transport.Digest(data)
			limited.artifactPath, err = buildkitepipeline.StagePath(limited.digest)
			if err != nil {
				t.Fatal(err)
			}
			runner := joinedContinueRunner(t, limited, "success", "success")
			code, _, stderr := runContinue(t, runner, limited.digest)
			if budget == 5 {
				if code != 1 || !strings.Contains(stderr, "may upload at most 5 jobs") || pipelineUploads(runner) != 0 {
					t.Fatalf("over-budget code=%d: %s", code, stderr)
				}
			} else if code != 0 || pipelineUploads(runner) != 1 {
				t.Fatalf("at-budget code=%d: %s", code, stderr)
			}
		})
	}
}

// continueChainedWorkflow chains two needs-derived matrices: build's rows come
// from plan, and deploy's rows come from package, a job that needs build and
// so exists only after build's deferred upload compiled it. release joins
// both matrices through publish and deploy.
const continueChainedWorkflow = continueDeferredMatrixWorkflow + `  package:
    needs: build
    runs-on: ubuntu-latest
    outputs:
      targets: ${{ steps.targets.outputs.targets }}
    steps:
      - id: targets
        run: true
  deploy:
    needs: package
    runs-on: ${{ matrix.runner }}
    strategy:
      matrix:
        include: ${{ fromJSON(needs.package.outputs.targets) }}
    steps:
      - run: true
  release:
    needs: [publish, deploy]
    runs-on: ubuntu-latest
    steps:
      - run: true
`

const (
	continuePackageJobID     = "0192f7d0-0000-4000-8000-00000000bbb4"
	continueSecondStageJobID = "0192f7d0-0000-4000-8000-00000000ccc2"
)

// continueStage is one deferred upload of a chained component: the artifact
// its step reads and the job that wrote it.
type continueStage struct {
	initial continueInitialUpload
	// writer is the job whose artifacts hold the stage's continuation.
	writer string
	// runner is the job the stage runs as.
	runner string
}

func firstStage(initial continueInitialUpload) continueStage {
	return continueStage{initial: initial, writer: continueImporterJobID, runner: continueContinuationJobID}
}

// run executes the stage against the verified result of its producer.
func (stage continueStage) run(t *testing.T, result, matrix string, producerJobID string, edit func(*cliCaptureRunner)) (*cliCaptureRunner, int, string, string) {
	t.Helper()
	manifest := stage.initial.producerManifestFrom(t, producerJobID, result, matrix)
	runner := &cliCaptureRunner{
		jobByStep:  map[string]string{stage.initial.artifact.rootProducer().Key: producerJobID},
		dataByPath: stage.initial.dataByPath(manifest),
	}
	if edit != nil {
		edit(runner)
	}
	code, stdout, stderr := runContinueAs(t, runner, stage.initial.digest, stage.writer, stage.runner)
	return runner, code, stdout, stderr
}

// nextStage returns the stage the runner's upload wrote for the rest of the
// component: exactly one child continuation. The event source stays with the
// importer, which every stage reads it from.
func (stage continueStage) nextStage(t *testing.T, runner *cliCaptureRunner) continueStage {
	t.Helper()
	next := continueInitialUpload{plans: map[string][]byte{}, eventPath: stage.initial.eventPath, eventData: stage.initial.eventData}
	for path, contents := range runner.uploaded {
		switch {
		case path == stage.initial.eventPath:
			t.Fatalf("stage re-uploaded the event source %s", path)
		case strings.HasPrefix(path, ".buildkite-gha/stages/"):
			if next.data != nil {
				t.Fatalf("stage wrote two continuations: %v", slices.Sorted(maps.Keys(runner.uploaded)))
			}
			artifact, err := decodeStageRecord(contents, "dev")
			if err != nil {
				t.Fatal(err)
			}
			next.artifactPath, next.data, next.digest, next.artifact = path, contents, transport.Digest(contents), artifact
		case strings.HasPrefix(path, ".buildkite-gha/plans/"):
			next.plans[path] = contents
		}
	}
	if next.data == nil {
		t.Fatalf("stage did not write a child continuation: %v", slices.Sorted(maps.Keys(runner.uploaded)))
	}
	next.pipeline = string(lastPipelineUpload(t, runner))
	if !strings.Contains(next.pipeline, "--stage-digest '"+next.digest+"'") || !strings.Contains(next.pipeline, "--stage-producer '"+stage.runner+"'") {
		t.Fatalf("pipeline does not run the child continuation %s from job %s:\n%s", next.digest, stage.runner, next.pipeline)
	}
	return continueStage{initial: next, writer: stage.runner, runner: continueSecondStageJobID}
}

func stepKeys(steps []continuePipelineStep) []string {
	keys := make([]string, 0, len(steps))
	for _, step := range steps {
		keys = append(keys, step.Key)
	}
	slices.Sort(keys)
	return keys
}

func dependencyKeys(step continuePipelineStep) []string {
	keys := make([]string, 0, len(step.DependsOn))
	for _, dependency := range step.DependsOn {
		keys = append(keys, dependency.Step)
	}
	slices.Sort(keys)
	return keys
}

// TestContinueChainsStages expands a component whose second matrix comes
// from a job the first deferred upload compiles. The importer writes one
// continuation for the component; its step compiles the jobs whose rows
// exist and adds a child step that reads the rows package publishes; the
// child step compiles the rest against the graph the first stage left.
func TestContinueChainsStages(t *testing.T) {
	initials := runContinueInitialUploads(t, continueChainedWorkflow)
	if len(initials) != 1 {
		t.Fatalf("continuations = %d, want one for the whole component", len(initials))
	}
	first := firstStage(initials[0])
	parent := first.initial.artifact
	prefix := strings.TrimSuffix(parent.rootProducer().Key, "plan")
	if !slices.Equal(parent.Continuation.Jobs, []string{"build", "package", "deploy", "publish", "release"}) {
		t.Fatalf("component jobs = %v", parent.Continuation.Jobs)
	}
	if !slices.Equal(parent.Continuation.LaterMatrices, []string{"deploy"}) {
		t.Fatalf("later matrices = %v", parent.Continuation.LaterMatrices)
	}
	if strings.Contains(first.initial.pipeline, `key: "`+prefix+`deploy-matrix"`) {
		t.Fatalf("initial upload created the second stage's step before its rows can exist:\n%s", first.initial.pipeline)
	}
	// lint and plan are static; the one component gets everything else, and
	// its stages share that budget between them.
	if parent.Continuation.JobBudget != compiler.MaxRuntimeMatrixGraphJobs-2 {
		t.Fatalf("component budget = %d", parent.Continuation.JobBudget)
	}

	buildRows := `[{"target":"amd64","runner":"ubuntu-latest"},{"target":"arm64","runner":"ubuntu-latest"}]`
	runner, code, stdout, stderr := first.run(t, "success", buildRows, continueProducerJobID, nil)
	if code != 0 {
		t.Fatalf("first stage code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `Uploaded 4 jobs for "build"`) || !strings.Contains(stdout, `Step "`+prefix+`deploy-matrix" expands "deploy", "release" once these jobs have run.`) {
		t.Fatalf("first stage stdout = %q", stdout)
	}
	_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	var childStep continuePipelineStep
	var buildKeys []string
	for _, step := range steps {
		switch {
		case step.Key == prefix+"deploy-matrix":
			childStep = step
		case strings.HasPrefix(step.Key, prefix+"build-"):
			buildKeys = append(buildKeys, step.Key)
		case step.Key != prefix+"package" && step.Key != prefix+"publish":
			t.Fatalf("first stage uploaded %q, which needs rows that do not exist yet", step.Key)
		}
	}
	if len(steps) != 5 || len(buildKeys) != 2 || childStep.Key == "" || childStep.Skip != "" {
		t.Fatalf("first stage steps = %v", stepKeys(steps))
	}
	if !slices.Equal(dependencyKeys(childStep), []string{prefix + "package"}) || !strings.Contains(childStep.Command, "upload --stage-digest") {
		t.Fatalf("child step = %#v, want a continuation that waits for package", childStep)
	}
	plans := uploadedPlans(t, runner)
	if len(plans) != 3 || len(plans["build"]) != 2 || len(plans["package"]) != 1 || len(plans["publish"]) != 1 {
		t.Fatalf("first stage uploaded plans for %v", slices.Sorted(maps.Keys(plans)))
	}
	firstSteps := steps

	second := first.nextStage(t, runner)
	child := second.initial.artifact
	if child.Importer != continueImporterJobID || child.Continuation.Descriptor.Job != "deploy" || child.Continuation.StepKey != prefix+"deploy-matrix" || !slices.Equal(child.Continuation.Jobs, []string{"deploy", "release"}) || len(child.Continuation.LaterMatrices) != 0 {
		t.Fatalf("child continuation = %+v", child.Continuation)
	}
	packagePlan, err := buildkitepipeline.PlanPath(child.rootProducer().PlanDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, uploaded := second.initial.plans[packagePlan]; child.rootProducer().Key != prefix+"package" || !uploaded {
		t.Fatalf("child producer = %+v, want the package plan this stage uploaded %v", child.rootProducer(), slices.Sorted(maps.Keys(second.initial.plans)))
	}
	if len(child.Resolved) != 1 || child.Resolved[0].Job != "build" || len(child.Resolved[0].Rows) != 2 || child.Resolved[0].Skipped {
		t.Fatalf("child resolved matrices = %+v", child.Resolved)
	}
	if !reflect.DeepEqual(child.Continuation.ActionLocks, parent.Continuation.ActionLocks) || child.Workflow != parent.Workflow || child.Event != parent.Event {
		t.Fatalf("child records different inputs than the importer: %+v", child)
	}
	graph := make([]string, 0, len(child.Graph))
	for _, job := range child.Graph {
		graph = append(graph, job.Key)
	}
	slices.Sort(graph)
	slices.Sort(buildKeys)
	if want := slices.Sorted(slices.Values(append([]string{prefix + "lint", prefix + "plan", prefix + "package", prefix + "publish"}, buildKeys...))); !slices.Equal(graph, want) {
		t.Fatalf("child graph = %v, want %v", graph, want)
	}
	if 4+child.Continuation.JobBudget != parent.Continuation.JobBudget {
		t.Fatalf("first stage uploaded 4 jobs and left %d to the child, parent budget %d", child.Continuation.JobBudget, parent.Continuation.JobBudget)
	}

	deployRows := `[{"target":"staging","runner":"ubuntu-latest"},{"target":"production","runner":"ubuntu-latest"},{"target":"canary","runner":"ubuntu-latest"}]`
	runner, code, stdout, stderr = second.run(t, "success", deployRows, continuePackageJobID, nil)
	if code != 0 {
		t.Fatalf("second stage code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `Expanding job "deploy" into 3 matrix instances.`) || !strings.Contains(stdout, `Uploaded 4 jobs for "deploy" from job "package" output "targets".`) || strings.Contains(stdout, "once these jobs have run") {
		t.Fatalf("second stage stdout = %q", stdout)
	}
	if !slices.Equal(runner.commands[0].args[:3], []string{"artifact", "download", second.initial.artifactPath}) || runner.commands[0].args[5] != continueContinuationJobID {
		t.Fatalf("child continuation was not read from the first stage: %#v", runner.commands[0])
	}
	_, _, steps = decodeContinuePipeline(t, lastPipelineUpload(t, runner))
	var deployKeys []string
	var release continuePipelineStep
	for _, step := range steps {
		switch {
		case strings.HasPrefix(step.Key, prefix+"deploy-"):
			deployKeys = append(deployKeys, step.Key)
			if !slices.Equal(dependencyKeys(step), []string{prefix + "package"}) {
				t.Fatalf("deploy instance %q depends on %v", step.Key, dependencyKeys(step))
			}
		case step.Key == prefix+"release":
			release = step
		default:
			t.Fatalf("second stage uploaded %q again", step.Key)
		}
	}
	if len(steps) != 4 || len(deployKeys) != 3 || release.Key == "" {
		t.Fatalf("second stage steps = %v", stepKeys(steps))
	}
	slices.Sort(deployKeys)
	// release joins the first stage's publish with this stage's instances.
	if want := slices.Sorted(slices.Values(append([]string{prefix + "publish"}, deployKeys...))); !slices.Equal(dependencyKeys(release), want) {
		t.Fatalf("release depends on %v, want %v", dependencyKeys(release), want)
	}
	plans = uploadedPlans(t, runner)
	if len(plans) != 2 || len(plans["deploy"]) != 3 || len(plans["release"]) != 1 {
		t.Fatalf("second stage uploaded plans for %v", slices.Sorted(maps.Keys(plans)))
	}
	for path := range runner.uploaded {
		if strings.HasPrefix(path, ".buildkite-gha/stages/") {
			t.Fatalf("last stage wrote a continuation %s", path)
		}
		if _, earlier := second.initial.plans[path]; earlier {
			t.Fatalf("second stage re-uploaded plan %s", path)
		}
	}

	t.Run("replay of the first stage", func(t *testing.T) {
		existing := map[string]map[string]string{}
		for _, step := range firstSteps {
			existing[step.Key] = map[string]string{"command": step.Command}
		}
		if !strings.Contains(existing[prefix+"deploy-matrix"]["command"], "'"+second.initial.digest+"'") {
			t.Fatalf("child step does not name the child artifact: %q", existing[prefix+"deploy-matrix"]["command"])
		}
		replay, code, stdout, stderr := first.run(t, "success", buildRows, continueProducerJobID, func(replay *cliCaptureRunner) {
			replay.pipelineUploadErr = errors.New("pipeline upload: duplicate step key")
			replay.stepAttributes = existing
		})
		if code != 0 || !strings.Contains(stdout, "were already uploaded by an earlier run of this step") || pipelineUploads(replay) != 1 {
			t.Fatalf("replay code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		// A child step bound to another artifact is not this stage's upload.
		existing[prefix+"deploy-matrix"] = map[string]string{"command": strings.Replace(existing[prefix+"deploy-matrix"]["command"], second.initial.digest, first.initial.digest, 1)}
		_, code, _, stderr = first.run(t, "success", buildRows, continueProducerJobID, func(replay *cliCaptureRunner) {
			replay.pipelineUploadErr = errors.New("pipeline upload: duplicate step key")
			replay.stepAttributes = existing
		})
		if code != 1 || !strings.Contains(stderr, stageRetryGuidance) {
			t.Fatalf("replay with another child accepted: code = %d, stderr = %q", code, stderr)
		}
	})
	t.Run("replay of the second stage", func(t *testing.T) {
		existing := map[string]map[string]string{}
		for _, step := range steps {
			existing[step.Key] = map[string]string{"command": step.Command}
		}
		replay, code, stdout, stderr := second.run(t, "success", deployRows, continuePackageJobID, func(replay *cliCaptureRunner) {
			replay.pipelineUploadErr = errors.New("pipeline upload: duplicate step key")
			replay.stepAttributes = existing
		})
		if code != 0 || !strings.Contains(stdout, "were already uploaded by an earlier run of this step") || pipelineUploads(replay) != 1 {
			t.Fatalf("replay code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		delete(existing, deployKeys[0])
		_, code, _, stderr = second.run(t, "success", deployRows, continuePackageJobID, func(replay *cliCaptureRunner) {
			replay.pipelineUploadErr = errors.New("pipeline upload: duplicate step key")
			replay.stepAttributes = existing
		})
		if code != 1 || !strings.Contains(stderr, stageRetryGuidance) {
			t.Fatalf("partial replay accepted: code = %d, stderr = %q", code, stderr)
		}
	})
	t.Run("first stage rows leave room for the later stage", func(t *testing.T) {
		// A budget of 5 holds this stage's 4 jobs, but deploy's placeholder
		// and release count against the rows before anything is compiled.
		limited := parent
		limited.Continuation.JobBudget = 5
		stage := first
		stage.initial = reencodeContinuation(t, first.initial, limited)
		runner, code, _, stderr := stage.run(t, "success", buildRows, continueProducerJobID, nil)
		if code != 1 || !strings.Contains(stderr, "may upload at most 5 jobs (1 rows plus their dependents)") || pipelineUploads(runner) != 0 {
			t.Fatalf("under-funded first stage code = %d, stderr = %q", code, stderr)
		}
	})
	t.Run("second stage rows are bounded by what the first stage left", func(t *testing.T) {
		// The child's budget covers its rows plus release.
		for budget, want := range map[int]int{3: 1, 4: 0} {
			limited := child
			limited.Continuation.JobBudget = budget
			stage := second
			stage.initial = reencodeContinuation(t, second.initial, limited)
			runner, code, _, stderr := stage.run(t, "success", deployRows, continuePackageJobID, nil)
			if code != want || want == 1 && (!strings.Contains(stderr, "may upload at most 3 jobs") || pipelineUploads(runner) != 0) {
				t.Fatalf("second stage with budget %d code = %d, stderr = %q", budget, code, stderr)
			}
		}
	})
	t.Run("second stage needs the first stage's resolved rows", func(t *testing.T) {
		// A child recording other build rows cannot reproduce the build
		// instances the first stage uploaded, so its recompilation drifts.
		forgetful := child
		forgetful.Resolved = []resolvedMatrix{{Job: "build", Rows: child.Resolved[0].Rows[:1]}}
		stage := second
		stage.initial = reencodeContinuation(t, second.initial, forgetful)
		runner, code, _, stderr := stage.run(t, "success", deployRows, continuePackageJobID, nil)
		if code != 1 || !strings.Contains(stderr, "the workflow inputs changed") || pipelineUploads(runner) != 0 {
			t.Fatalf("child with other resolved rows code = %d, stderr = %q", code, stderr)
		}
		// Without any resolved rows, build is still deferred in the
		// recompilation and deploy's rows cannot be joined to it.
		forgetful.Resolved = nil
		stage.initial = reencodeContinuation(t, second.initial, forgetful)
		if _, code, _, stderr := stage.run(t, "success", deployRows, continuePackageJobID, nil); code != 1 || !strings.Contains(stderr, "compile deferred jobs") {
			t.Fatalf("child without resolved rows code = %d, stderr = %q", code, stderr)
		}
	})
	t.Run("second stage rejects rows its parent promised to a later stage", func(t *testing.T) {
		// The child owns deploy; a child that still lists deploy as a later
		// matrix while expanding it contradicts itself.
		contradictory := child
		contradictory.Continuation.LaterMatrices = []string{"deploy"}
		if _, err := decodeStageRecord(mustJSON(t, contradictory), "dev"); err == nil {
			t.Fatal("accepted a continuation that both expands and defers deploy")
		}
	})
}

// reencodeContinuation replaces the artifact of an upload with an edited one,
// re-addressing it by digest so the deferred step accepts the bytes.
func reencodeContinuation(t *testing.T, initial continueInitialUpload, artifact stageRecord) continueInitialUpload {
	t.Helper()
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	initial.artifact, initial.data, initial.digest = artifact, data, transport.Digest(data)
	initial.artifactPath, err = buildkitepipeline.StagePath(initial.digest)
	if err != nil {
		t.Fatal(err)
	}
	return initial
}

// TestContinueChainsThreeStages proves a chain has no fixed depth: every
// stage's step writes the next stage's artifact until no matrix remains, the
// rows resolved so far accumulate, and the stages together spend exactly the
// component's budget.
func TestContinueChainsThreeStages(t *testing.T) {
	source := continueChainedWorkflow + `  verify:
    needs: deploy
    runs-on: ubuntu-latest
    outputs:
      channels: ${{ steps.channels.outputs.channels }}
    steps:
      - id: channels
        run: true
  announce:
    needs: [verify, lint]
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include: ${{ fromJSON(needs.verify.outputs.channels) }}
    steps:
      - run: true
`
	stage := firstStage(runContinueInitialUploads(t, source)[0])
	parent := stage.initial.artifact
	prefix := strings.TrimSuffix(parent.rootProducer().Key, "plan")
	if got := parent.Continuation.LaterMatrices; !slices.Equal(got, []string{"deploy", "announce"}) {
		t.Fatalf("later matrices = %v", got)
	}
	rows := map[string]string{
		"build":    `[{"target":"x","runner":"ubuntu-latest"}]`,
		"deploy":   `[{"target":"eu","runner":"ubuntu-latest"},{"target":"us","runner":"ubuntu-latest"}]`,
		"announce": `[{"channel":"stable","runner":"ubuntu-latest"}]`,
	}
	producers := map[string]string{"build": continueProducerJobID, "deploy": continuePackageJobID, "announce": "0192f7d0-0000-4000-8000-00000000bbb5"}
	var expanded []string
	total := 0
	for len(expanded) < len(rows) {
		job := stage.initial.artifact.Continuation.Descriptor.Job
		budget := stage.initial.artifact.Continuation.JobBudget
		runner, code, stdout, stderr := stage.run(t, "success", rows[job], producers[job], nil)
		if code != 0 {
			t.Fatalf("stage %q code = %d, stdout = %q, stderr = %q", job, code, stdout, stderr)
		}
		expanded = append(expanded, job)
		uploaded := 0
		for _, plans := range uploadedPlans(t, runner) {
			uploaded += len(plans)
		}
		total += uploaded
		var resolved []string
		for _, matrix := range stage.initial.artifact.Resolved {
			resolved = append(resolved, matrix.Job)
		}
		if !slices.Equal(resolved, expanded[:len(expanded)-1]) {
			t.Fatalf("stage %q recorded rows for %v, want the earlier stages %v", job, resolved, expanded[:len(expanded)-1])
		}
		if len(expanded) == len(rows) {
			for path := range runner.uploaded {
				if strings.HasPrefix(path, ".buildkite-gha/stages/") {
					t.Fatalf("last stage %q wrote a continuation %s", job, path)
				}
			}
			if uploaded > budget || total > parent.Continuation.JobBudget {
				t.Fatalf("stages uploaded %d jobs in total and %d in the last, budgets %d and %d", total, uploaded, parent.Continuation.JobBudget, budget)
			}
			break
		}
		next := stage.nextStage(t, runner)
		if next.initial.artifact.Continuation.JobBudget != budget-uploaded {
			t.Fatalf("stage %q uploaded %d jobs from budget %d and left %d", job, uploaded, budget, next.initial.artifact.Continuation.JobBudget)
		}
		// Each stage runs as a new job; the runner ID only matters for the
		// artifact producer the next stage reads from.
		next.runner = fmt.Sprintf("0192f7d0-0000-4000-8000-00000000ccc%d", len(expanded)+1)
		stage = next
	}
	if !slices.Equal(expanded, []string{"build", "deploy", "announce"}) {
		t.Fatalf("stages expanded %v", expanded)
	}
	// The last stage's step key was reserved by the initial compilation and
	// depends on the job the second stage compiled.
	if stage.initial.artifact.Continuation.StepKey != prefix+"announce-matrix" || stage.initial.artifact.rootProducer().Key != prefix+"verify" {
		t.Fatalf("last stage = %+v", stage.initial.artifact.Continuation)
	}
}

// TestContinueChainedStagesSkipWhenProducersFail proves each stage of a chain
// resolves the jobs it cannot run. A failed first producer skips the whole
// component, including the matrix a later stage would have expanded, under
// one placeholder each; a failed second producer skips only the jobs the
// second stage owns, and the first stage's jobs stay as uploaded.
func TestContinueChainedStagesSkipWhenProducersFail(t *testing.T) {
	first := firstStage(runContinueInitialUploads(t, continueChainedWorkflow)[0])
	prefix := strings.TrimSuffix(first.initial.artifact.rootProducer().Key, "plan")

	t.Run("first producer fails", func(t *testing.T) {
		runner, code, stdout, stderr := first.run(t, "failure", "", continueProducerJobID, nil)
		if code != 0 || !strings.Contains(stdout, `finished with result "failure"; the deferred jobs are skipped`) {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if len(runner.uploaded) != 0 {
			t.Fatalf("skipped component uploaded artifacts: %v", slices.Sorted(maps.Keys(runner.uploaded)))
		}
		_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
		if want := []string{prefix + "build", prefix + "deploy", prefix + "package", prefix + "publish", prefix + "release"}; !slices.Equal(stepKeys(steps), want) {
			t.Fatalf("skipped steps = %v, want the whole component under one placeholder each %v", stepKeys(steps), want)
		}
		for _, step := range steps {
			if !strings.Contains(step.Skip, `matrix producer job "plan" finished with result failure`) || !slices.Equal(dependencyKeys(step), []string{prefix + "plan"}) {
				t.Fatalf("skipped step = %#v", step)
			}
		}
	})

	t.Run("second producer fails", func(t *testing.T) {
		runner, code, _, stderr := first.run(t, "success", `[{"target":"x","runner":"ubuntu-latest"}]`, continueProducerJobID, nil)
		if code != 0 {
			t.Fatalf("first stage code = %d, stderr = %q", code, stderr)
		}
		second := first.nextStage(t, runner)
		runner, code, stdout, stderr := second.run(t, "failure", "", continuePackageJobID, nil)
		if code != 0 || !strings.Contains(stdout, `Matrix producer "package" finished with result "failure"`) {
			t.Fatalf("second stage code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if len(runner.uploaded) != 0 {
			t.Fatalf("skipped stage uploaded artifacts: %v", slices.Sorted(maps.Keys(runner.uploaded)))
		}
		_, _, steps := decodeContinuePipeline(t, lastPipelineUpload(t, runner))
		if want := []string{prefix + "deploy", prefix + "release"}; !slices.Equal(stepKeys(steps), want) {
			t.Fatalf("skipped steps = %v, want only the second stage's jobs", stepKeys(steps))
		}
		for _, step := range steps {
			if !strings.Contains(step.Skip, `matrix producer job "package" finished with result failure`) || !slices.Equal(dependencyKeys(step), []string{prefix + "package"}) {
				t.Fatalf("skipped step = %#v", step)
			}
		}
		// Skipping is idempotent when the placeholders already exist.
		replay, code, stdout, stderr := second.run(t, "failure", "", continuePackageJobID, func(replay *cliCaptureRunner) {
			replay.pipelineUploadErr = errors.New("pipeline upload: duplicate step key")
			replay.stepAttributes = map[string]map[string]string{}
			for _, step := range steps {
				replay.stepAttributes[step.Key] = map[string]string{"label": step.Label}
			}
		})
		if code != 0 || !strings.Contains(stdout, "already uploaded") || pipelineUploads(replay) != 1 {
			t.Fatalf("skip replay code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
	})
}

// TestContinueRejectsMalformedResolvedMatrices proves a child artifact's
// recorded rows are checked the way live producer output is before they
// reach the compiler.
func TestContinueRejectsMalformedResolvedMatrices(t *testing.T) {
	first := firstStage(runContinueInitialUploads(t, continueChainedWorkflow)[0])
	runner, code, _, stderr := first.run(t, "success", `[{"target":"x","runner":"ubuntu-latest"}]`, continueProducerJobID, nil)
	if code != 0 {
		t.Fatalf("first stage code = %d, stderr = %q", code, stderr)
	}
	child := first.nextStage(t, runner).initial.artifact
	if _, err := decodeStageRecord(mustJSON(t, child), "dev"); err != nil {
		t.Fatalf("intact child rejected: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(*stageRecord)
	}{
		{name: "rows for a skipped matrix", edit: func(a *stageRecord) { a.Resolved[0].Skipped = true }},
		{name: "no rows and not skipped", edit: func(a *stageRecord) { a.Resolved[0].Rows = nil }},
		{name: "nested row value", edit: func(a *stageRecord) { a.Resolved[0].Rows[0]["runner"] = []any{"ubuntu-latest"} }},
		{name: "empty row", edit: func(a *stageRecord) { a.Resolved[0].Rows[0] = map[string]any{} }},
		{name: "job still deferred", edit: func(a *stageRecord) { a.Resolved[0].Job = "deploy" }},
		{name: "unnamed job", edit: func(a *stageRecord) { a.Resolved[0].Job = "" }},
		{name: "duplicate job", edit: func(a *stageRecord) { a.Resolved = append(a.Resolved, a.Resolved[0]) }},
		{name: "too many rows", edit: func(a *stageRecord) {
			for len(a.Resolved[0].Rows) <= compiler.MaxRuntimeMatrixInstances {
				a.Resolved[0].Rows = append(a.Resolved[0].Rows, a.Resolved[0].Rows[0])
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			edited, err := decodeStageRecord(mustJSON(t, child), "dev")
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&edited)
			if _, err := decodeStageRecord(mustJSON(t, edited), "dev"); err == nil {
				t.Fatal("accepted malformed resolved matrices")
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestStageRecordRoundTrip proves the artifact decodes strictly and
// records the values the continuation needs.
func TestStageRecordRoundTrip(t *testing.T) {
	initial := runContinueInitialUpload(t, "--runner-queue", "ubuntu-latest=custom-linux")
	artifact := initial.artifact
	if artifact.Schema != stageSchema || artifact.Importer != continueImporterJobID || artifact.Workflow.Path != ".github/workflows/build.yml" {
		t.Fatalf("artifact = %+v", artifact)
	}
	// --event-path supplies a payload file, which every plan digest records.
	if artifact.Event.Name != "push" || !artifact.Event.File {
		t.Fatalf("artifact event = %+v", artifact.Event)
	}
	if len(artifact.Runners) != 1 || artifact.Runners[0].Label != "ubuntu-latest" || artifact.Runners[0].Queue != "custom-linux" {
		t.Fatalf("artifact runners = %+v", artifact.Runners)
	}
	if len(artifact.Graph) != 2 || artifact.Graph[0].LogicalJob != "lint" || artifact.Graph[1].LogicalJob != "plan" {
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
	if _, err := decodeStageRecord(extra, "dev"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	delete(loose, "extra")
	delete(loose["continuation"].(map[string]any), "sources")
	withoutSources, _ := json.Marshal(loose)
	if _, err := decodeStageRecord(withoutSources, "dev"); err == nil || !strings.Contains(err.Error(), "must record the source of every deferred job") {
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
		if _, err := decodeStageRecord(withBudget, "dev"); err == nil || !strings.Contains(err.Error(), "records an invalid job budget") {
			t.Fatalf("job budget %d error = %v", budget, err)
		}
	}
	if _, err := decodeStageRecord(initial.data, "other"); err == nil || !strings.Contains(err.Error(), "written by buildkite-gha dev") {
		t.Fatalf("version error = %v", err)
	}
	// The workflow path must stay inside the checkout; ".." within a filename
	// is a valid name, a ".." segment or an absolute path is not.
	withPath := func(workflowPath string) []byte {
		return bytes.Replace(initial.data, []byte(`".github/workflows/build.yml"`), []byte(`"`+workflowPath+`"`), 1)
	}
	for _, valid := range []string{".github/workflows/ci..matrix.yml", ".github/workflows/..build.yml"} {
		if _, err := decodeStageRecord(withPath(valid), "dev"); err != nil {
			t.Fatalf("path %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"../build.yml", ".github/../../build.yml", "/etc/build.yml", ".github//workflows/build.yml", ""} {
		if _, err := decodeStageRecord(withPath(invalid), "dev"); err == nil || !strings.Contains(err.Error(), "invalid workflow path") {
			t.Fatalf("path %q error = %v", invalid, err)
		}
	}
}

// TestContinuationArtifactRebuildsTheImporterRequest proves that the deferred
// upload compiles the request the importer recorded: the same runner
// mapping, runtime digests, variables, OIDC, event exposure, and namespace,
// with the verified rows supplied for the deferred job and every
// continuation's action locks pinned. The live inputs the importer cannot
// record, runner resolution and the environment source, stay unset.
func TestContinuationArtifactRebuildsTheImporterRequest(t *testing.T) {
	oidc := &plan.OIDCConfiguration{Claims: []string{"repository"}}
	cache := &buildkitepipeline.CacheVolume{Paths: []string{"~/.cache/go-build"}, Name: "go", Size: "10g"}
	ownLock := plan.ActionLock{ID: "actions/checkout@v4", Source: "github", Repository: "actions/checkout", RequestedRef: "v4", Commit: strings.Repeat("a", 40)}
	otherLock := plan.ActionLock{ID: "./.github/actions/setup", Source: "workspace", Path: ".github/actions/setup"}
	artifact := stageRecord{
		Version:      "dev",
		Distribution: "sha256:" + strings.Repeat("d", 64),
		Runtimes:     map[string]string{compiler.PlatformLinuxAMD64.String(): "sha256:" + strings.Repeat("1", 64), compiler.PlatformDarwinARM64.String(): "sha256:" + strings.Repeat("2", 64)},
		Workflow:     stageWorkflow{Path: ".github/workflows/build.yml", Namespace: "build"},
		Event:        stageEvent{Name: "push", Provider: "github", File: true},
		Runners:      []stageRunner{{Label: "ubuntu-latest", Queue: "custom-linux", Platform: compiler.PlatformLinuxAMD64.String(), Image: "ubuntu", Cache: cache}},
		Vars:         stageVars{Organization: map[string]string{"REGION": "us-east-1"}, Repository: map[string]string{"TEAM": "pipelines"}, Resolved: true},
		OIDC:         oidc,
		Continuation: compiler.RuntimeContinuation{Descriptor: compiler.RuntimeOutputDescriptor{Job: "test"}, ActionLocks: []plan.ActionLock{ownLock}},
		Others:       []compiler.RuntimeContinuation{{Descriptor: compiler.RuntimeOutputDescriptor{Job: "publish"}, ActionLocks: []plan.ActionLock{otherLock}}},
	}
	rows := []map[string]any{{"os": "ubuntu-latest"}, {"os": "macos-latest"}}
	source := compiler.MemoizeRepositorySource(nil)
	got := artifact.compileRequest("/checkout/.github/workflows/build.yml", []byte("on: push\n"), []byte("{}"), map[string][]map[string]any{"test": rows}, nil, nil, source)
	want := hostedCompileRequest{
		WorkflowPath:       "/checkout/.github/workflows/build.yml",
		WorkflowSource:     []byte("on: push\n"),
		EventSource:        []byte("{}"),
		EventFile:          true,
		Version:            "dev",
		DistributionDigest: artifact.Distribution,
		ImporterStep:       "pipeline-trigger-importer",
		StepKeyNamespace:   "build",
		RunnerTargets:      map[string]compiler.RunnerTarget{"ubuntu-latest": {Queue: "custom-linux", Platform: compiler.PlatformLinuxAMD64, Image: "ubuntu", Cache: cache}},
		RuntimeDistributions: map[compiler.Platform]string{
			compiler.PlatformLinuxAMD64:  artifact.Runtimes[compiler.PlatformLinuxAMD64.String()],
			compiler.PlatformDarwinARM64: artifact.Runtimes[compiler.PlatformDarwinARM64.String()],
		},
		OIDC:                     oidc,
		Vars:                     compiler.VariableSources{Organization: map[string]string{"REGION": "us-east-1"}, Repository: map[string]string{"TEAM": "pipelines"}, Resolved: true},
		RepositorySource:         source,
		RuntimeMatrixRows:        map[string][]map[string]any{"test": rows},
		RuntimeMatrixSkipped:     map[string]bool{},
		RuntimeMatrixActionLocks: []plan.ActionLock{ownLock, otherLock},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compileRequest() = %#v\nwant %#v", got, want)
	}
	// The rows shape validation as well as compilation, so a row naming a
	// runner outside the policy is rejected before any plan is built.
	if options := got.validationOptions(); !reflect.DeepEqual(options.RuntimeMatrixRows, want.RuntimeMatrixRows) {
		t.Fatalf("validation rows = %#v", options.RuntimeMatrixRows)
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
