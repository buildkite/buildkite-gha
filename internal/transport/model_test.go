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

func TestUploadArtifactsIdentifiesPipelineFailure(t *testing.T) {
	artifact := Artifact{Path: ".buildkite-gha/plans/plan.json", Contents: []byte("plan")}
	artifact.Digest = Digest(artifact.Contents)
	err := UploadArtifacts(context.Background(), Agent{Runner: &captureRunner{failAt: 2}}, t.TempDir(), []Artifact{artifact}, []byte("steps: []\n"))
	if !errors.Is(err, ErrPipelineUpload) || !strings.Contains(err.Error(), "upload pipeline") {
		t.Fatalf("UploadArtifacts() error = %v", err)
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
