package transport

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	testBuildID = "33333333-3333-4333-8333-333333333333"
	testJobID   = "44444444-4444-4444-8444-444444444444"
	testJobID2  = "55555555-5555-4555-8555-555555555555"
)

func TestEmitTwoJobPipelineIsDeterministicAndKeepsDependencyPoliciesSeparate(t *testing.T) {
	producer := testPlan("gha-producer", `{"job":"producer"}`)
	consumer := testPlan("gha-consumer", `{"job":"consumer"}`)
	jobs := []Job{
		{Key: "gha-producer", Label: "Producer", Command: "probe producer", Queue: "gha-runtime", Plan: producer},
		{Key: "gha-consumer", Label: "Consumer", Command: "probe consumer", Queue: "gha-runtime", Plan: consumer, Dependencies: []Dependency{{StepKey: "gha-producer"}}},
	}
	first, err := EmitTwoJobPipeline("gha-importer", jobs)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EmitTwoJobPipeline("gha-importer", jobs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("pipeline emission was not byte-identical")
	}
	want := `steps:
  - label: "Producer"
    key: "gha-producer"
    command: "probe producer"
    agents:
      queue: "gha-runtime"
    checkout:
      skip: true
    env:
      BUILDKITE_GHA_PLAN_DIGEST: "` + producer.Digest + `"
      BUILDKITE_GHA_PLAN_PRODUCER: "gha-importer"
    depends_on:
      - step: "gha-importer"
        allow_failure: false
  - label: "Consumer"
    key: "gha-consumer"
    command: "probe consumer"
    agents:
      queue: "gha-runtime"
    checkout:
      skip: true
    env:
      BUILDKITE_GHA_PLAN_DIGEST: "` + consumer.Digest + `"
      BUILDKITE_GHA_PLAN_PRODUCER: "gha-importer"
    depends_on:
      - step: "gha-importer"
        allow_failure: false
      - step: "gha-producer"
        allow_failure: true
`
	if string(first) != want {
		t.Fatalf("pipeline changed\nwant:\n%s\ngot:\n%s", want, first)
	}
}

func TestEmitTwoJobPipelineRejectsForwardNeed(t *testing.T) {
	producer := testPlan("gha-producer", `{}`)
	consumer := testPlan("gha-consumer", `{}`)
	jobs := []Job{
		{Key: "gha-producer", Label: "Producer", Command: "true", Queue: "runtime", Plan: producer, Dependencies: []Dependency{{StepKey: "gha-consumer"}}},
		{Key: "gha-consumer", Label: "Consumer", Command: "true", Queue: "runtime", Plan: consumer},
	}
	_, err := EmitTwoJobPipeline("gha-importer", jobs)
	if err == nil || !strings.Contains(err.Error(), "unknown, forward, or duplicate need") {
		t.Fatalf("error = %v", err)
	}
}

func TestResultManifestIsCanonicalAndProducerBound(t *testing.T) {
	producer := Producer{BuildID: testBuildID, JobID: testJobID, StepKey: "gha-producer"}
	manifest := ResultManifest{
		PlanDigest: Digest([]byte("plan")),
		Producer:   producer,
		Result:     "failure",
		Outputs:    []Output{{Name: "zeta", Value: "last"}, {Name: "alpha", Value: "first"}},
	}
	encoded, err := MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"outputs":[{"name":"alpha"`) {
		t.Fatalf("outputs were not sorted: %s", encoded)
	}
	verified, err := VerifyResultManifest(encoded, manifest.PlanDigest, producer)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Result != "failure" {
		t.Fatalf("result = %q", verified.Result)
	}
	if _, err := VerifyResultManifest(encoded, manifest.PlanDigest, Producer{BuildID: testBuildID, JobID: testJobID2, StepKey: "gha-producer"}); err == nil {
		t.Fatal("manifest was accepted for the wrong producer job")
	}
	metadata, err := ResultMetadata("ci", "producer", verified, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if metadata["buildkite-gha/v1/ci/producer/result"] != "failure" || metadata["buildkite-gha/v1/ci/producer/manifest-digest"] != Digest(encoded) {
		t.Fatalf("metadata = %#v", metadata)
	}
	if _, err := ResultMetadata("ci/other", "producer", verified, encoded); err == nil {
		t.Fatal("untrusted metadata namespace was accepted")
	}
}

type capturedCommand struct {
	name  string
	args  []string
	stdin string
}

type captureRunner struct {
	commands []capturedCommand
	failAt   int
}

func (r *captureRunner) Run(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, capturedCommand{name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if r.failAt != 0 && len(r.commands) == r.failAt {
		return nil, errors.New("injected failure")
	}
	return nil, nil
}

func TestAgentUsesExactProducerAndSafeUploadFlags(t *testing.T) {
	runner := &captureRunner{}
	agent := Agent{Runner: runner}
	if err := agent.DownloadArtifact(context.Background(), "result.json", ".", "gha-producer"); err != nil {
		t.Fatal(err)
	}
	if err := agent.UploadPipeline(context.Background(), []byte("steps: []\n")); err != nil {
		t.Fatal(err)
	}
	want := []capturedCommand{
		{name: "buildkite-agent", args: []string{"artifact", "download", "result.json", ".", "--step", "gha-producer"}},
		{name: "buildkite-agent", args: []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets", "--reject-parse-warnings"}, stdin: "steps: []\n"},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
}

func TestUploadOrdersArtifactsMarkersAndPipelineAndFailsClosed(t *testing.T) {
	key := testKey(t)
	producer := testPlan("gha-producer", `{"job":"producer"}`)
	consumer := testPlan("gha-consumer", `{"job":"consumer"}`)
	pipeline := []byte("steps: []\n")
	intent := UploadIntent{
		BuildID: testBuildID, ImporterKey: "gha-importer", PipelineDigest: Digest(pipeline),
		Jobs: []UploadJob{{Key: producer.StepKey, PlanDigest: producer.Digest}, {Key: consumer.StepKey, PlanDigest: consumer.Digest}},
	}
	expected, err := SignMarker(key, intent, "expected")
	if err != nil {
		t.Fatal(err)
	}
	completed, err := SignMarker(key, intent, "completed")
	if err != nil {
		t.Fatal(err)
	}
	runner := &captureRunner{}
	if err := Upload(context.Background(), Agent{Runner: runner}, []PlanArtifact{consumer, producer}, pipeline, expected, completed); err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"artifact", "artifact", "artifact", "artifact", "meta-data", "pipeline", "meta-data"}
	for i, kind := range wantKinds {
		if runner.commands[i].args[0] != kind {
			t.Fatalf("command %d = %#v, want %s", i, runner.commands[i], kind)
		}
	}
	if runner.commands[0].args[2] != consumer.Path() || runner.commands[1].args[2] != consumer.BindingPath() || runner.commands[2].args[2] != producer.Path() || runner.commands[3].args[2] != producer.BindingPath() {
		t.Fatalf("plans and bindings not sorted before upload: %#v", runner.commands[:4])
	}

	failing := &captureRunner{failAt: 6}
	err = Upload(context.Background(), Agent{Runner: failing}, []PlanArtifact{producer, consumer}, pipeline, expected, completed)
	if err == nil || len(failing.commands) != 6 {
		t.Fatalf("Upload() error = %v, commands = %d; completion marker must not be published", err, len(failing.commands))
	}

	missing := &captureRunner{}
	err = Upload(context.Background(), Agent{Runner: missing}, []PlanArtifact{producer}, pipeline, expected, completed)
	if err == nil || len(missing.commands) != 0 {
		t.Fatalf("Upload() error = %v, commands = %d; missing signed plan set must fail before side effects", err, len(missing.commands))
	}
}

func testPlan(stepKey, contents string) PlanArtifact {
	data := []byte(contents)
	return PlanArtifact{StepKey: stepKey, Digest: Digest(data), Contents: data, Binding: []byte("signed-binding")}
}

func testKey(t *testing.T) ES256Key {
	t.Helper()
	key, err := NewTestES256Key("phase0-test")
	if err != nil {
		t.Fatal(err)
	}
	return key
}
