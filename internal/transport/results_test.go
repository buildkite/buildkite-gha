package transport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type resultRunner struct {
	commands     []capturedCommand
	jobByStep    map[string]string
	dataByPath   map[string][]byte
	failMetadata bool
}

func (r *resultRunner) Run(_ context.Context, dir, name string, args []string, stdin []byte) ([]byte, error) {
	r.commands = append(r.commands, capturedCommand{dir: dir, name: name, args: append([]string(nil), args...), stdin: string(stdin)})
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "search" {
		return []byte(r.jobByStep[args[4]] + "\n"), nil
	}
	if len(args) >= 2 && args[0] == "artifact" && args[1] == "download" {
		contents, ok := r.dataByPath[args[2]]
		if !ok {
			return nil, errors.New("missing fixture artifact")
		}
		path := filepath.Join(args[3], filepath.FromSlash(args[2]))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(path, contents, 0o600)
	}
	if r.failMetadata && len(args) >= 2 && args[0] == "meta-data" && args[1] == "set" {
		return nil, errors.New("metadata unavailable")
	}
	return nil, nil
}

func TestPublishResultCoversTerminalConclusionsAndKeepsMetadataAdvisory(t *testing.T) {
	for _, conclusion := range []string{"success", "failure", "skipped"} {
		t.Run(conclusion, func(t *testing.T) {
			runner := &resultRunner{failMetadata: true}
			manifest := resultManifest(testJobID, "gha-producer", Digest([]byte("plan")), conclusion, Output{Name: "message", Value: "bounded"})
			publication, err := PublishResult(context.Background(), Agent{Runner: runner}, t.TempDir(), "shell", "producer", manifest)
			if err != nil {
				t.Fatal(err)
			}
			if publication.Path != ResultPath("gha-producer", manifest.PlanDigest) || publication.MetadataMirrorError == nil {
				t.Fatalf("publication = %#v, want authoritative artifact plus advisory metadata error", publication)
			}
			if len(runner.commands) < 2 || !reflect.DeepEqual(runner.commands[0].args, []string{"artifact", "upload", publication.Path}) {
				t.Fatalf("commands = %#v, authoritative artifact must be uploaded first", runner.commands)
			}
			for _, command := range runner.commands {
				if len(command.args) > 1 && command.args[0] == "meta-data" && command.args[1] == "get" {
					t.Fatal("publication read metadata as authority")
				}
			}
		})
	}
}

func TestDownloadResultSelectsExactStepThenExactProducerJob(t *testing.T) {
	planDigest := Digest([]byte("producer-plan"))
	path := ResultPath("gha-producer", planDigest)
	encoded := mustManifest(t, resultManifest(testJobID, "gha-producer", planDigest, "success", Output{Name: "message", Value: "ok"}))
	runner := &resultRunner{
		jobByStep:  map[string]string{"gha-producer": testJobID},
		dataByPath: map[string][]byte{path: encoded},
	}
	manifest, err := DownloadResult(context.Background(), Agent{Runner: runner}, t.TempDir(), testBuildID, ResultSource{StepKey: "gha-producer", PlanDigest: planDigest})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Outputs[0].Value != "ok" || len(runner.commands) != 2 {
		t.Fatalf("manifest/commands = %#v / %#v", manifest, runner.commands)
	}
	wantSearch := []string{"artifact", "search", path, "--step", "gha-producer", "--format", "%j"}
	if !reflect.DeepEqual(runner.commands[0].args, wantSearch) {
		t.Fatalf("search = %#v, want %#v", runner.commands[0].args, wantSearch)
	}
	wantDownloadSuffix := []string{"--step", testJobID}
	if !reflect.DeepEqual(runner.commands[1].args[len(runner.commands[1].args)-2:], wantDownloadSuffix) {
		t.Fatalf("download = %#v, want exact producer UUID", runner.commands[1].args)
	}
	for _, command := range runner.commands {
		if len(command.args) > 0 && command.args[0] == "meta-data" {
			t.Fatal("result download consulted non-authoritative metadata")
		}
	}
}

func TestDownloadResultRejectsTamperWrongProducerAndSize(t *testing.T) {
	planDigest := Digest([]byte("producer-plan"))
	path := ResultPath("gha-producer", planDigest)
	valid := mustManifest(t, resultManifest(testJobID, "gha-producer", planDigest, "success"))
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "tampered", data: append(append([]byte(nil), valid...), ' '), want: "not canonical"},
		{name: "wrong producer", data: mustManifest(t, resultManifest(testJobID2, "gha-producer", planDigest, "success")), want: "producer or plan binding mismatch"},
		{name: "oversized", data: bytes.Repeat([]byte("x"), MaxResultManifestBytes+1), want: "not a regular bounded manifest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &resultRunner{jobByStep: map[string]string{"gha-producer": testJobID}, dataByPath: map[string][]byte{path: test.data}}
			_, err := DownloadResult(context.Background(), Agent{Runner: runner}, t.TempDir(), testBuildID, ResultSource{StepKey: "gha-producer", PlanDigest: planDigest})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DownloadResult() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadNeedsMapsLogicalFanInToVerifiedProducers(t *testing.T) {
	first := ResultSource{StepKey: "gha-build-one", PlanDigest: Digest([]byte("one-plan"))}
	second := ResultSource{StepKey: "gha-build-two", PlanDigest: Digest([]byte("two-plan"))}
	firstPath := ResultPath(first.StepKey, first.PlanDigest)
	secondPath := ResultPath(second.StepKey, second.PlanDigest)
	runner := &resultRunner{
		jobByStep: map[string]string{first.StepKey: testJobID, second.StepKey: testJobID2},
		dataByPath: map[string][]byte{
			firstPath:  mustManifest(t, resultManifest(testJobID, first.StepKey, first.PlanDigest, "success", Output{Name: "first", Value: "one"})),
			secondPath: mustManifest(t, resultManifest(testJobID2, second.StepKey, second.PlanDigest, "failure", Output{Name: "second", Value: "two"})),
		},
	}
	needs, err := LoadNeeds(context.Background(), Agent{Runner: runner}, t.TempDir(), testBuildID, map[string][]ResultSource{"build": {second, first}})
	if err != nil {
		t.Fatal(err)
	}
	if needs["build"].Result != "failure" || !reflect.DeepEqual(needs["build"].Outputs, map[string]string{"first": "one", "second": "two"}) || len(needs["build"].Producers) != 2 {
		t.Fatalf("needs = %#v", needs)
	}

	runner.dataByPath[secondPath] = mustManifest(t, resultManifest(testJobID2, second.StepKey, second.PlanDigest, "success", Output{Name: "first", Value: "different"}))
	_, err = LoadNeeds(context.Background(), Agent{Runner: runner}, t.TempDir(), testBuildID, map[string][]ResultSource{"build": {first, second}})
	if err == nil || !strings.Contains(err.Error(), "conflicting matrix output") {
		t.Fatalf("LoadNeeds() error = %v, want ambiguous matrix output rejection", err)
	}
}

func TestAggregateResultTreatsMixedSuccessAndSkippedAsSuccess(t *testing.T) {
	if got := aggregateResult("skipped", "success"); got != "success" {
		t.Fatalf("aggregateResult() = %q, want success", got)
	}
	if got := aggregateResult("success", "cancelled"); got != "cancelled" {
		t.Fatalf("aggregateResult() = %q, want cancelled", got)
	}
	if got := aggregateResult("cancelled", "failure"); got != "failure" {
		t.Fatalf("aggregateResult() = %q, want failure", got)
	}
}

func resultManifest(jobID, stepKey, planDigest, result string, outputs ...Output) ResultManifest {
	return ResultManifest{
		PlanDigest: planDigest,
		Producer:   Producer{BuildID: testBuildID, JobID: jobID, StepKey: stepKey},
		Result:     result,
		Outputs:    outputs,
	}
}

func mustManifest(t *testing.T, manifest ResultManifest) []byte {
	t.Helper()
	encoded, err := MarshalResultManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
