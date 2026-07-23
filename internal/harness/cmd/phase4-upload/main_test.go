package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/buildkite"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"github.com/buildkite/buildkite-gha/internal/transport"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type capturedUpload struct {
	path     string
	contents []byte
}

type captureRunner struct {
	uploads      []capturedUpload
	pipeline     []byte
	pipelineArgs []string
}

func (r *captureRunner) Run(_ context.Context, dir, _ string, args []string, stdin []byte) ([]byte, error) {
	if len(args) >= 3 && args[0] == "artifact" && args[1] == "upload" {
		contents, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(args[2])))
		if err != nil {
			return nil, err
		}
		r.uploads = append(r.uploads, capturedUpload{path: args[2], contents: contents})
	}
	if len(args) >= 2 && args[0] == "pipeline" && args[1] == "upload" {
		r.pipeline = bytes.Clone(stdin)
		r.pipelineArgs = append([]string(nil), args...)
	}
	return nil, nil
}

func TestParseArgsRequiresProofInputs(t *testing.T) {
	valid := []string{"--event-path", "event.json", "--runtime", "runtime", "--runtime-version", "v1", "--runtime-queue", "hosted", "--node24", "node", "--commit", strings.Repeat("a", 40), "workflow.yml"}
	if got, err := parseArgs(valid); err != nil || got.workflow != "workflow.yml" {
		t.Fatalf("parseArgs() = %#v, %v", got, err)
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{"missing", valid[:len(valid)-1]},
		{"queue", append(append([]string(nil), valid[:7]...), append([]string{"other"}, valid[8:]...)...)},
		{"commit", append(append([]string(nil), valid[:11]...), append([]string{"ABC"}, valid[12:]...)...)},
		{"extra workflow", append(valid, "other.yml")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseArgs(test.args); err == nil {
				t.Fatal("parseArgs() succeeded")
			}
		})
	}
}

func TestDeterministicGzip(t *testing.T) {
	first, err := deterministicGzip([]byte("node executable"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := deterministicGzip([]byte("node executable"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("gzip output changed")
	}
	r, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "node executable" {
		t.Fatalf("contents = %q", got)
	}
}

func TestBindCommitStrictlyRewritesSnapshot(t *testing.T) {
	original := []byte(`{"provider":"github","event":"push","repository":{"owner":"o","name":"r","clone_url":"https://example.test/o/r","default_branch":"main"},"ref":"refs/heads/main","sha":"old","actor":"a","payload":{"number":1}}`)
	bound, err := bindCommit(original, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		SHA     string         `json:"sha"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(bound, &got); err != nil {
		t.Fatal(err)
	}
	if got.SHA != strings.Repeat("b", 40) {
		t.Fatalf("sha = %q", got.SHA)
	}
	if !bytes.Contains(original, []byte(`"sha":"old"`)) {
		t.Fatal("input was mutated")
	}
	for _, malformed := range [][]byte{
		append(append([]byte(nil), original...), []byte(` {}`)...),
		[]byte(`{"provider":"github","unknown":true}`),
		[]byte(`{`),
	} {
		if _, err := bindCommit(malformed, strings.Repeat("b", 40)); err == nil {
			t.Fatalf("bindCommit(%q) succeeded", malformed)
		}
	}
}

func TestRunUploadsTrustedV3BundleWithManagedNode(t *testing.T) {
	repository := t.TempDir()
	workflowPath := filepath.Join(repository, ".github", "workflows", "test.yml")
	actionPath := filepath.Join(repository, ".github", "actions", "javascript")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/javascript\n")
	if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: node24\n  main: index.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "index.js"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputs := t.TempDir()
	eventPath := filepath.Join(inputs, "event.json")
	event := []byte(`{"provider":"github","event":"push","repository":{"owner":"buildkite","name":"buildkite-gha","clone_url":"https://github.com/buildkite/buildkite-gha.git","default_branch":"main"},"ref":"refs/heads/main","sha":"1111111111111111111111111111111111111111","actor":"phase4","payload":{"ref":"refs/heads/main"}}`)
	if err := os.WriteFile(eventPath, event, 0o600); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(inputs, "buildkite-gha")
	nodePath := filepath.Join(inputs, "node24")
	runtimeBytes, nodeBytes := []byte("exact runtime"), []byte("exact node 24")
	if err := os.WriteFile(runtimePath, runtimeBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodePath, nodeBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	args := []string{
		"--event-path", eventPath,
		"--runtime", runtimePath,
		"--runtime-version", "0.0.0-phase4.test",
		"--runtime-queue", "hosted",
		"--node24", nodePath,
		"--commit", commit,
		workflowPath,
	}
	getenv := func(name string) string {
		return map[string]string{"BUILDKITE": "true", "BUILDKITE_STEP_KEY": "phase-4-upload-importer"}[name]
	}
	runner := &captureRunner{}
	var stdout bytes.Buffer
	if err := run(context.Background(), args, &stdout, transport.Agent{Runner: runner}, getenv); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Uploaded 1 jobs with importer phase-4-upload-importer.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(runner.uploads) != 3 || len(runner.pipeline) == 0 {
		t.Fatalf("uploads = %#v, pipeline bytes = %d", runner.uploads, len(runner.pipeline))
	}
	if got := runner.uploads[0]; got.path != mustDistributionPath(t, transport.Digest(runtimeBytes)) || !bytes.Equal(got.contents, runtimeBytes) {
		t.Fatalf("runtime upload = %#v", got)
	}
	var jobPlan plan.Job
	for _, upload := range runner.uploads {
		if strings.HasSuffix(upload.path, ".json") {
			var planDocument any
			if err := json.Unmarshal(upload.contents, &planDocument); err != nil {
				t.Fatal(err)
			}
			schemaSource, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "schemas", "job-plan-v3.schema.json"))
			if err != nil {
				t.Fatal(err)
			}
			var schemaDocument any
			if err := json.Unmarshal(schemaSource, &schemaDocument); err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			if err := compiler.AddResource(plan.SchemaV3, schemaDocument); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(plan.SchemaV3)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(planDocument); err != nil {
				t.Fatalf("Phase 4 plan does not validate against v3 schema: %v\n%s", err, upload.contents)
			}
			decoded, err := plan.Decode(upload.contents)
			if err != nil {
				t.Fatal(err)
			}
			jobPlan = decoded
		}
	}
	if jobPlan.Schema != plan.SchemaV3 || jobPlan.Compiler.Version != "0.0.0-phase4.test" || jobPlan.Event.SHA != commit || len(jobPlan.Actions) != 1 || jobPlan.Actions[0].Source != "workspace" {
		t.Fatalf("trusted plan = %#v", jobPlan)
	}
	nodeArchive, err := deterministicGzip(nodeBytes)
	if err != nil {
		t.Fatal(err)
	}
	nodeDigest := transport.Digest(nodeArchive)
	nodeArtifactPath, err := buildkite.NodeRuntimePath(24, nodeDigest)
	if err != nil {
		t.Fatal(err)
	}
	if got := runner.uploads[2]; got.path != nodeArtifactPath || !bytes.Equal(got.contents, nodeArchive) {
		t.Fatalf("Node upload = %#v", got)
	}
	pipeline := string(runner.pipeline)
	for _, required := range []string{nodeArtifactPath, nodeDigest, "BUILDKITE_GHA_NODE24", "--step 'phase-4-upload-importer'"} {
		if !strings.Contains(pipeline, required) {
			t.Fatalf("generated pipeline lacks %q:\n%s", required, pipeline)
		}
	}
	if strings.Join(runner.pipelineArgs, " ") != "pipeline upload --no-interpolation --reject-secrets" {
		t.Fatalf("pipeline upload args = %#v", runner.pipelineArgs)
	}
	unchanged, err := os.ReadFile(eventPath)
	if err != nil || !bytes.Equal(unchanged, event) {
		t.Fatalf("event file changed: %v", err)
	}
}

func TestRunRequiresBuildkiteImporterIdentity(t *testing.T) {
	args := []string{"--event-path", "event", "--runtime", "runtime", "--runtime-version", "v1", "--runtime-queue", "hosted", "--node24", "node", "--commit", strings.Repeat("a", 40), "workflow"}
	err := run(context.Background(), args, io.Discard, transport.Agent{}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "BUILDKITE and BUILDKITE_STEP_KEY") {
		t.Fatalf("run() error = %v", err)
	}
}

func mustDistributionPath(t *testing.T, digest string) string {
	t.Helper()
	path, err := buildkite.DistributionPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
