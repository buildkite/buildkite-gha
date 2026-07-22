package compiler

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

const testDistributionDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestCompileBundleGoldenAndDeterministic(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	source := readFile(t, path)
	event := readFile(t, smokePath("events", "push.json"))
	first, err := CompileBundle(path, source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileBundle(path, source, event, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated bundle compilation was not byte-identical")
	}
	if len(first.Plans) != 3 || len(first.IR.Jobs) != 3 {
		t.Fatalf("bundle jobs/plans = %d/%d, want 3/3", len(first.IR.Jobs), len(first.Plans))
	}
	producer := first.IR.Jobs[0].Key
	for i, artifact := range first.Plans {
		if artifact.Path != ".buildkite-gha/plans/"+strings.TrimPrefix(artifact.Digest, "sha256:")+".json" {
			t.Fatalf("plan %d path = %q", i, artifact.Path)
		}
		if i == 0 && len(artifact.Job.Dependencies) != 0 {
			t.Fatalf("producer dependencies = %#v", artifact.Job.Dependencies)
		}
		if i > 0 && !reflect.DeepEqual(artifact.Job.Dependencies, []string{producer}) {
			t.Fatalf("consumer dependencies = %#v, want %q", artifact.Job.Dependencies, producer)
		}
	}
	wantPipeline := readFile(t, filepath.Join("testdata", "shell.pipeline.golden.yml"))
	if !bytes.Equal(first.Pipeline, wantPipeline) {
		t.Fatalf("pipeline changed\nwant:\n%s\ngot:\n%s", wantPipeline, first.Pipeline)
	}
	wantPlans := readFile(t, filepath.Join("testdata", "shell.plans.golden.json"))
	if !bytes.Equal(encodeGoldenPlans(t, first.Plans), wantPlans) {
		t.Fatalf("plans changed; update shell.plans.golden.json intentionally")
	}
}

func TestCompileBundleCompilesSmokeCorpus(t *testing.T) {
	workflows, err := filepath.Glob(smokePath(".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) != 4 {
		t.Fatalf("smoke workflows = %d, want 4", len(workflows))
	}
	event := readFile(t, smokePath("events", "push.json"))
	for _, path := range workflows {
		t.Run(filepath.Base(path), func(t *testing.T) {
			bundle, err := CompileBundle(path, readFile(t, path), event, "0.0.0-test", testDistributionDigest, "gha-importer")
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Plans) == 0 || len(bundle.Pipeline) == 0 {
				t.Fatalf("empty bundle: %#v", bundle)
			}
			var document map[string]any
			if err := yaml.Unmarshal(bundle.Pipeline, &document); err != nil {
				t.Fatalf("pipeline YAML: %v", err)
			}
		})
	}
}

func TestCompileBundleDoesNotExposeEventValues(t *testing.T) {
	path := smokePath(".github", "workflows", "shell.yml")
	var event map[string]any
	if err := json.Unmarshal(readFile(t, smokePath("events", "push.json")), &event); err != nil {
		t.Fatal(err)
	}
	event["payload"].(map[string]any)["private_fixture_value"] = "super-secret-value"
	eventSource, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := CompileBundle(path, readFile(t, path), eventSource, "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bundle.Pipeline, []byte("super-secret-value")) {
		t.Fatal("pipeline contains an event payload value")
	}
	for _, artifact := range bundle.Plans {
		if bytes.Contains(artifact.Contents, []byte("super-secret-value")) {
			t.Fatalf("plan %q contains an event payload value", artifact.Job.Target.StepKey)
		}
	}
}

func encodeGoldenPlans(t *testing.T, artifacts []PlanArtifact) []byte {
	t.Helper()
	plans := make([]json.RawMessage, len(artifacts))
	for i, artifact := range artifacts {
		plans[i] = json.RawMessage(bytes.TrimSpace(artifact.Contents))
	}
	encoded, err := json.MarshalIndent(plans, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}
