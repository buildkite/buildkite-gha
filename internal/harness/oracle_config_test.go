package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestShellOracleDefinitionsMatchFixtureContract(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "testdata", "smoke", ".github", "workflows", "shell.yml")
	fixture := readYAMLMap(t, fixturePath)
	jobs := yamlMap(t, fixture["jobs"], "jobs")
	producer := yamlMap(t, jobs["producer"], "producer")
	consumer := yamlMap(t, jobs["consumer"], "consumer")

	expectedPath := filepath.Join("..", "..", "testdata", "smoke", shellExpectedCapturePath)
	expectedBytes, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatal(err)
	}
	var expected normalizedCapture
	if err := json.Unmarshal(expectedBytes, &expected); err != nil {
		t.Fatal(err)
	}
	if len(expected.Observations) == 0 {
		t.Fatal("shell capture expectation has no observations")
	}

	variantsRaw, ok := yamlMap(t, yamlMap(t, consumer["strategy"], "strategy")["matrix"], "matrix")["variant"].([]any)
	if !ok {
		t.Fatalf("consumer matrix variants = %#v, want list", consumer)
	}
	wantIdentities := make(map[string]bool, len(variantsRaw))
	for _, raw := range variantsRaw {
		variant, ok := raw.(string)
		if !ok {
			t.Fatalf("matrix variant = %#v, want string", raw)
		}
		wantIdentities["consumer[variant="+variant+"]"] = false
	}

	result := ""
	for _, observation := range expected.Observations {
		if _, ok := wantIdentities[observation.Identity]; !ok {
			t.Fatalf("expected capture contains identity %q absent from shell.yml matrix", observation.Identity)
		}
		wantIdentities[observation.Identity] = true
		var document struct {
			Result string `json:"result"`
		}
		if err := json.Unmarshal(observation.Document, &document); err != nil {
			t.Fatal(err)
		}
		if result == "" {
			result = document.Result
		} else if document.Result != result {
			t.Fatalf("shell capture results disagree: %q and %q", result, document.Result)
		}
	}
	for identity, found := range wantIdentities {
		if !found {
			t.Fatalf("shell.yml matrix identity %q is absent from expected capture", identity)
		}
	}

	producerBytes, err := yaml.Marshal(producer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(producerBytes), "result="+result) {
		t.Fatalf("producer does not emit expected result %q", result)
	}
	for _, path := range []string{
		filepath.Join("..", "..", ".github", "workflows", "phase-0-shell-oracle.yml"),
		filepath.Join("..", "..", ".buildkite", "phase-0-shell-oracle.yml"),
	} {
		definition, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(definition), result) {
			t.Fatalf("%s does not carry fixture result %q", path, result)
		}
		for _, raw := range variantsRaw {
			if !strings.Contains(string(definition), raw.(string)) {
				t.Fatalf("%s does not carry fixture variant %q", path, raw)
			}
		}
	}
}

func TestGitHubShellOracleDefinitionIsManualAndPinned(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "phase-0-shell-oracle.yml")
	document := readYAMLMap(t, path)
	triggers := yamlMap(t, document["on"], "on")
	if len(triggers) != 1 || triggers["workflow_dispatch"] == nil {
		t.Fatalf("on = %#v, want workflow_dispatch only", triggers)
	}
	inputs := yamlMap(t, yamlMap(t, triggers["workflow_dispatch"], "workflow_dispatch")["inputs"], "inputs")
	sourceCommit, ok := inputs["source_commit"]
	if !ok {
		t.Fatal("workflow_dispatch does not require source_commit")
	}
	if required := yamlMap(t, sourceCommit, "source_commit")["required"]; required != true {
		t.Fatalf("source_commit required = %#v, want true", required)
	}
	assertDefinitionText(t, path, []string{
		"internal/harness/cmd/shell-oracle materialize",
		"internal/harness/cmd/shell-oracle compare",
		"--provider github",
		"scripts/phase-0-shell-oracle-checkout",
	})
}

func TestBuildkiteShellOracleDefinitionHasIsolatedDependencyGraph(t *testing.T) {
	path := filepath.Join("..", "..", ".buildkite", "phase-0-shell-oracle.yml")
	document := readYAMLMap(t, path)
	steps, ok := document["steps"].([]any)
	if !ok || len(steps) != 5 {
		t.Fatalf("steps = %#v, want five command steps", document["steps"])
	}
	wantKeys := map[string]bool{
		"phase-0-shell-prepare":      false,
		"phase-0-shell-producer":     false,
		"phase-0-shell-consumer-one": false,
		"phase-0-shell-consumer-two": false,
		"phase-0-shell-compare":      false,
	}
	for index, raw := range steps {
		step := yamlMap(t, raw, "step")
		key, ok := step["key"].(string)
		if !ok {
			t.Fatalf("step %d has no string key", index)
		}
		if _, ok := wantKeys[key]; !ok {
			t.Fatalf("unexpected step key %q", key)
		}
		wantKeys[key] = true
		if _, ok := step["command"]; !ok {
			t.Fatalf("step %q is not a command step", key)
		}
	}
	for key, found := range wantKeys {
		if !found {
			t.Errorf("missing step %q", key)
		}
	}
	assertDefinitionText(t, path, []string{
		"internal/harness/cmd/shell-oracle materialize",
		"internal/harness/cmd/shell-oracle compare",
		"--provider buildkite",
		"scripts/phase-0-shell-oracle-checkout",
		"$$ORACLE_SOURCE_COMMIT",
	})
	documentBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(documentBytes), `"$ORACLE_SOURCE_COMMIT"`) {
		t.Fatal("Buildkite source commit reference would be interpolated during pipeline upload")
	}
}

func readYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return decoded
}

func yamlMap(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want map", name, value)
	}
	return result
}

func assertDefinitionText(t *testing.T, path string, fragments []string) {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(document)
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Errorf("%s does not contain %q", path, fragment)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "uses:") {
			continue
		}
		reference := strings.Fields(strings.TrimPrefix(line, "uses:"))[0]
		_, revision, ok := strings.Cut(reference, "@")
		if !ok || len(revision) != 40 || strings.Trim(revision, "0123456789abcdef") != "" {
			t.Errorf("action use is not pinned to a full lowercase commit: %q", line)
		}
	}
}
