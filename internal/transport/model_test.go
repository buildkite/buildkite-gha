package transport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	if metadata["buildkite-gha/v1/results/ci/producer"] != "failure" || metadata["buildkite-gha/v1/results/ci/producer/manifest-digest"] != Digest(encoded) || metadata["buildkite-gha/v1/outputs/ci/producer/alpha"] != "first" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if _, err := ResultMetadata("ci/other", "producer", verified, encoded); err == nil {
		t.Fatal("untrusted metadata namespace was accepted")
	}
}

func TestResultManifestEnforcesOutputLimits(t *testing.T) {
	manifest := ResultManifest{
		PlanDigest: Digest([]byte("plan")),
		Producer:   Producer{BuildID: testBuildID, JobID: testJobID, StepKey: "gha-producer"},
		Result:     "success",
		Outputs:    []Output{{Name: "value", Value: strings.Repeat("x", MaxResultOutputBytes+1)}},
	}
	if _, err := MarshalResultManifest(manifest); err == nil || !strings.Contains(err.Error(), "oversized output") {
		t.Fatalf("MarshalResultManifest() error = %v, want output size rejection", err)
	}
	manifest.Outputs = make([]Output, MaxResultOutputs+1)
	if _, err := MarshalResultManifest(manifest); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("MarshalResultManifest() error = %v, want output count rejection", err)
	}
	manifest.Outputs = []Output{{Name: "Value", Value: "one"}, {Name: "value", Value: "two"}}
	if _, err := MarshalResultManifest(manifest); err == nil || !strings.Contains(err.Error(), "duplicate output") {
		t.Fatalf("MarshalResultManifest() error = %v, want case-insensitive duplicate rejection", err)
	}
}

func TestResultManifestArtifactsAreCanonicalAndRoundTrip(t *testing.T) {
	manifest := resultManifest(testJobID, "gha-producer", Digest([]byte("plan")), "success")
	manifest.Artifacts = []ResultArtifact{
		resultArtifact("zeta", "20", strings.Repeat("b", 64)),
		resultArtifact("alpha", "10", strings.Repeat("a", 64)),
	}
	encoded, err := MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"schema":"buildkite-gha/result-manifest/v2"`) ||
		!strings.Contains(string(encoded), `"artifacts":[{"name":"alpha","id":"10"`) {
		t.Fatalf("manifest schema or artifact order is not canonical: %s", encoded)
	}
	verified, err := VerifyResultManifest(encoded, manifest.PlanDigest, manifest.Producer)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(verified.Artifacts, []ResultArtifact{manifest.Artifacts[1], manifest.Artifacts[0]}) {
		t.Fatalf("artifacts = %#v", verified.Artifacts)
	}

	manifest.Artifacts = nil
	encoded, err = MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"artifacts":[]`) {
		t.Fatalf("nil artifacts were not normalized: %s", encoded)
	}
}

func TestResultManifestRejectsMalformedArtifacts(t *testing.T) {
	base := resultArtifact("artifact", "1", strings.Repeat("a", 64))
	tests := []struct {
		name   string
		mutate func(*ResultArtifact)
	}{
		{name: "empty name", mutate: func(a *ResultArtifact) { a.Name = "" }},
		{name: "invalid UTF-8 name", mutate: func(a *ResultArtifact) { a.Name = string([]byte{0xff}) }},
		{name: "oversized name", mutate: func(a *ResultArtifact) { a.Name = strings.Repeat("x", MaxResultArtifactNameBytes+1) }},
		{name: "forbidden name character", mutate: func(a *ResultArtifact) { a.Name = "directory/file" }},
		{name: "zero ID", mutate: func(a *ResultArtifact) { a.ID = "0" }},
		{name: "signed ID", mutate: func(a *ResultArtifact) { a.ID = "+1" }},
		{name: "oversized ID", mutate: func(a *ResultArtifact) { a.ID = strings.Repeat("1", MaxResultArtifactIDBytes+1) }},
		{name: "wrong path prefix", mutate: func(a *ResultArtifact) { a.Path = "other/" + strings.Repeat("a", 64) + ".zip" }},
		{name: "uppercase path digest", mutate: func(a *ResultArtifact) { a.Path = "buildkite-gha/v1/artifacts/" + strings.Repeat("A", 64) + ".zip" }},
		{name: "invalid digest", mutate: func(a *ResultArtifact) { a.Digest = strings.Repeat("a", 64) }},
		{name: "zero size", mutate: func(a *ResultArtifact) { a.Size = 0 }},
		{name: "oversized archive", mutate: func(a *ResultArtifact) { a.Size = MaxResultArtifactSizeBytes + 1 }},
		{name: "zero files", mutate: func(a *ResultArtifact) { a.FileCount = 0 }},
		{name: "too many files", mutate: func(a *ResultArtifact) { a.FileCount = MaxResultArtifactFileCount + 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := base
			test.mutate(&artifact)
			manifest := resultManifest(testJobID, "gha-producer", Digest([]byte("plan")), "success")
			manifest.Artifacts = []ResultArtifact{artifact}
			if _, err := MarshalResultManifest(manifest); err == nil {
				t.Fatal("MarshalResultManifest() accepted malformed artifact")
			}
		})
	}

	manifest := resultManifest(testJobID, "gha-producer", Digest([]byte("plan")), "success")
	manifest.Artifacts = make([]ResultArtifact, MaxResultArtifacts+1)
	if _, err := MarshalResultManifest(manifest); err == nil {
		t.Fatal("MarshalResultManifest() accepted too many artifacts")
	}
}

func TestResultManifestRejectsDuplicateArtifacts(t *testing.T) {
	first := resultArtifact("Artifact", "1", strings.Repeat("a", 64))
	for _, test := range []struct {
		name   string
		second ResultArtifact
	}{
		{name: "case-insensitive name", second: resultArtifact("artifact", "2", strings.Repeat("b", 64))},
		{name: "ID", second: resultArtifact("other", "1", strings.Repeat("b", 64))},
		{name: "path", second: resultArtifact("other", "2", strings.Repeat("a", 64))},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := resultManifest(testJobID, "gha-producer", Digest([]byte("plan")), "success")
			manifest.Artifacts = []ResultArtifact{first, test.second}
			if _, err := MarshalResultManifest(manifest); err == nil || !strings.Contains(err.Error(), "duplicate result artifact") {
				t.Fatalf("MarshalResultManifest() error = %v", err)
			}
		})
	}
}

func resultArtifact(name, id, hexDigest string) ResultArtifact {
	return ResultArtifact{
		Name: name, ID: id,
		Path: "buildkite-gha/v1/artifacts/" + hexDigest + ".zip", Digest: "sha256:" + hexDigest,
		Size: 1, FileCount: 1,
	}
}

type capturedCommand struct {
	dir   string
	name  string
	args  []string
	stdin string
}

type captureRunner struct {
	commands []capturedCommand
	failAt   int
	afterRun func(int)
}

func (r *captureRunner) Run(_ context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, capturedCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if r.failAt != 0 && len(r.commands) == r.failAt {
		return nil, errors.New("injected failure")
	}
	if r.afterRun != nil {
		r.afterRun(len(r.commands))
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
		{name: "buildkite-agent", args: []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}, stdin: "steps: []\n"},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
}

func TestAgentPublishesBoundedJobAnnotationThroughStdin(t *testing.T) {
	runner := &captureRunner{}
	agent := Agent{Runner: runner}
	body := "### Job summary\n\nPassed.\n"
	if err := agent.AnnotateJob(context.Background(), testJobID, "buildkite-gha-job-summary", "info", body); err != nil {
		t.Fatal(err)
	}
	want := capturedCommand{
		name:  "buildkite-agent",
		args:  []string{"annotate", "--scope", "job", "--job", testJobID, "--context", "buildkite-gha-job-summary", "--style", "info"},
		stdin: body,
	}
	if !reflect.DeepEqual(runner.commands, []capturedCommand{want}) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}

	for _, test := range []struct {
		name, jobID, context, style, body string
	}{
		{name: "invalid job", jobID: "not-a-job", context: "summary", style: "info", body: "body"},
		{name: "empty context", jobID: testJobID, style: "info", body: "body"},
		{name: "oversized context", jobID: testJobID, context: strings.Repeat("x", maxAnnotationContextCharacters+1), style: "info", body: "body"},
		{name: "invalid context UTF-8", jobID: testJobID, context: string([]byte{0xff}), style: "info", body: "body"},
		{name: "invalid style", jobID: testJobID, context: "summary", style: "default", body: "body"},
		{name: "empty body", jobID: testJobID, context: "summary", style: "info"},
		{name: "oversized body", jobID: testJobID, context: "summary", style: "info", body: strings.Repeat("x", maxAnnotationBodyBytes+1)},
		{name: "invalid UTF-8", jobID: testJobID, context: "summary", style: "info", body: string([]byte{0xff})},
	} {
		t.Run(test.name, func(t *testing.T) {
			validationRunner := &captureRunner{}
			err := (Agent{Runner: validationRunner}).AnnotateJob(context.Background(), test.jobID, test.context, test.style, test.body)
			if err == nil || len(validationRunner.commands) != 0 {
				t.Fatalf("AnnotateJob() error = %v, commands = %#v", err, validationRunner.commands)
			}
		})
	}
}

func TestUploadArtifactsMaterializesContentBeforePipeline(t *testing.T) {
	root := t.TempDir()
	plan := Artifact{Path: ".buildkite-gha/plans/plan.json", Contents: []byte("plan\n")}
	plan.Digest = Digest(plan.Contents)
	distribution := Artifact{Path: ".buildkite-gha/distributions/bin/buildkite-gha", Contents: []byte("binary")}
	distribution.Digest = Digest(distribution.Contents)
	runner := &captureRunner{}
	if err := UploadArtifacts(context.Background(), Agent{Runner: runner}, root, []Artifact{plan, distribution}, []byte("steps: []\n")); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("commands = %#v, want two artifacts then pipeline", runner.commands)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []capturedCommand{
		{dir: resolvedRoot, name: "buildkite-agent", args: []string{"artifact", "upload", distribution.Path}},
		{dir: resolvedRoot, name: "buildkite-agent", args: []string{"artifact", "upload", plan.Path}},
		{name: "buildkite-agent", args: []string{"pipeline", "upload", "--no-interpolation", "--reject-secrets"}, stdin: "steps: []\n"},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
	for _, artifact := range []Artifact{plan, distribution} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(artifact.Path)))
		if err != nil || !bytes.Equal(contents, artifact.Contents) {
			t.Fatalf("materialized %q = %q, %v", artifact.Path, contents, err)
		}
	}
}

func TestUploadArtifactsFailsBeforePipeline(t *testing.T) {
	artifact := Artifact{Path: ".buildkite-gha/plans/plan.json", Digest: Digest([]byte("plan")), Contents: []byte("plan")}
	tests := []struct {
		name      string
		artifacts []Artifact
		failAt    int
		wantRuns  int
	}{
		{name: "invalid digest", artifacts: []Artifact{{Path: artifact.Path, Digest: artifact.Digest, Contents: []byte("tampered")}}},
		{name: "duplicate path", artifacts: []Artifact{artifact, artifact}},
		{name: "artifact upload failure", artifacts: []Artifact{artifact}, failAt: 1, wantRuns: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &captureRunner{failAt: test.failAt}
			err := UploadArtifacts(context.Background(), Agent{Runner: runner}, t.TempDir(), test.artifacts, []byte("steps: []\n"))
			if err == nil {
				t.Fatal("UploadArtifacts() succeeded")
			}
			if len(runner.commands) != test.wantRuns {
				t.Fatalf("commands = %#v, want %d and no pipeline upload", runner.commands, test.wantRuns)
			}
		})
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
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(consumer.Path()))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(consumer.Path())), []byte("stale plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upload(context.Background(), Agent{Runner: runner}, root, []PlanArtifact{consumer, producer}, pipeline, expected, completed); err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"artifact", "artifact", "artifact", "artifact", "meta-data", "pipeline", "meta-data"}
	for i, kind := range wantKinds {
		if runner.commands[i].args[0] != kind {
			t.Fatalf("command %d = %#v, want %s", i, runner.commands[i], kind)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path := func(relative string) string { return filepath.Join(resolvedRoot, filepath.FromSlash(relative)) }
	if runner.commands[0].dir != resolvedRoot || runner.commands[1].dir != resolvedRoot || runner.commands[2].dir != resolvedRoot || runner.commands[3].dir != resolvedRoot || runner.commands[0].args[2] != consumer.Path() || runner.commands[1].args[2] != consumer.BindingPath() || runner.commands[2].args[2] != producer.Path() || runner.commands[3].args[2] != producer.BindingPath() {
		t.Fatalf("plans and bindings not sorted before upload: %#v", runner.commands[:4])
	}
	for _, plan := range []PlanArtifact{producer, consumer} {
		contents, err := os.ReadFile(path(plan.Path()))
		if err != nil || !bytes.Equal(contents, plan.Contents) {
			t.Fatalf("materialized plan %q = %q, %v", plan.StepKey, contents, err)
		}
		binding, err := os.ReadFile(path(plan.BindingPath()))
		if err != nil || !bytes.Equal(binding, plan.Binding) {
			t.Fatalf("materialized binding %q = %q, %v", plan.StepKey, binding, err)
		}
	}

	failing := &captureRunner{failAt: 6}
	err = Upload(context.Background(), Agent{Runner: failing}, t.TempDir(), []PlanArtifact{producer, consumer}, pipeline, expected, completed)
	if err == nil || len(failing.commands) != 6 {
		t.Fatalf("Upload() error = %v, commands = %d; completion marker must not be published", err, len(failing.commands))
	}

	missing := &captureRunner{}
	err = Upload(context.Background(), Agent{Runner: missing}, t.TempDir(), []PlanArtifact{producer}, pipeline, expected, completed)
	if err == nil || len(missing.commands) != 0 {
		t.Fatalf("Upload() error = %v, commands = %d; missing signed plan set must fail before side effects", err, len(missing.commands))
	}
}

func TestUploadRejectsTamperedMaterializedBindingBeforeAgentReadsIt(t *testing.T) {
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
	root := t.TempDir()
	tampered := filepath.Join(root, filepath.FromSlash(consumer.BindingPath()))
	runner := &captureRunner{afterRun: func(command int) {
		if command == 1 {
			if err := os.WriteFile(tampered, []byte("tampered"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}}
	err = Upload(context.Background(), Agent{Runner: runner}, root, []PlanArtifact{producer, consumer}, pipeline, expected, completed)
	if err == nil || !strings.Contains(err.Error(), "on-disk bytes differ") || len(runner.commands) != 1 {
		t.Fatalf("Upload() error = %v, commands = %d", err, len(runner.commands))
	}
}

func TestCommandRunnerBoundsOutputWhileReading(t *testing.T) {
	stdout, _, err := (CommandRunner{}).RunBounded(context.Background(), "", "sh", []string{"-c", "printf 123456789"}, nil, 8)
	if err == nil || !strings.Contains(err.Error(), "exceeds 8 bytes") || len(stdout) != 0 {
		t.Fatalf("runBounded() stdout = %q, error = %v", stdout, err)
	}
}

func TestGetMetadataBoundedClassifiesOnlyExpectedWebhookAbsence(t *testing.T) {
	for _, test := range []struct {
		name, message, stdout string
		exit                  int
		wantAbsent            bool
	}{
		{name: "non-webhook", message: "400 Bad Request: Build was not triggered by a webhook", exit: 1, wantAbsent: true},
		{name: "missing linked webhook", message: "404 Not Found: Build webhook is not available", exit: 1, wantAbsent: true},
		{name: "rate limited", message: "429 Too Many Requests: rate limit exceeded", exit: 1},
		{name: "unexpected exit", message: "400 Bad Request: Build was not triggered by a webhook", exit: 2},
		{name: "oversized output", message: "400 Bad Request: Build was not triggered by a webhook", stdout: strings.Repeat("x", 1025), exit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			agentPath := filepath.Join(dir, "buildkite-agent")
			script := "#!/bin/sh\nprintf '%s' '" + test.stdout + "'\nprintf '%s\\n' '" + test.message + "' >&2\nexit " + strconv.Itoa(test.exit) + "\n"
			if err := os.WriteFile(agentPath, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			_, err := (Agent{Runner: CommandRunner{}}).GetMetadataBounded(context.Background(), "buildkite:webhook", 1024)
			if got := errors.Is(err, ErrMetadataUnavailable); got != test.wantAbsent {
				t.Fatalf("GetMetadataBounded() error = %v, absent = %t, want %t", err, got, test.wantAbsent)
			}
			if !test.wantAbsent && err == nil {
				t.Fatalf("operational error = %v", err)
			}
		})
	}
}

func testPlan(stepKey, contents string) PlanArtifact {
	data := []byte(contents)
	return PlanArtifact{StepKey: stepKey, Digest: Digest(data), Contents: data, Binding: []byte("signed-binding")}
}

func testKey(t *testing.T) ES256Key {
	t.Helper()
	key, err := NewTestES256Key("transport-probe-test")
	if err != nil {
		t.Fatal(err)
	}
	return key
}
