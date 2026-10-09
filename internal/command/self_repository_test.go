package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/transport"
)

type selfReferenceSource struct {
	root   string
	digest string
	refs   []actionsource.Reference
}

func (s *selfReferenceSource) Fetch(_ context.Context, ref actionsource.Reference) (actionsource.Resolved, actionsource.Materialized, error) {
	s.refs = append(s.refs, ref)
	if ref.Owner+"/"+ref.Repository != "buildkite/buildkite-gha" || ref.Ref != strings.Repeat("1", 40) {
		return actionsource.Resolved{}, actionsource.Materialized{}, &actionsource.NotPublicError{}
	}
	return actionsource.Resolved{Reference: ref, Commit: ref.Ref}, actionsource.Materialized{RepositoryRoot: s.root, SourceDigest: s.digest}, nil
}

func TestSelfRepositoryProfileVerifiesCandidateAndRejectsSyntheticIdentity(t *testing.T) {
	for _, test := range []struct {
		name, event, uses, result string
		mismatch                  bool
	}{
		{name: "matching event candidate", uses: "$/action", result: "admitted"},
		{name: "matching event reusable", uses: "$/.github/workflows/called.yml", result: "admitted"},
		{name: "modified workflow", uses: "$/action", mismatch: true, result: "indeterminate"},
		{name: "synthetic event action", event: "push", uses: "$/action", result: "indeterminate"},
		{name: "synthetic event reusable", event: "push", uses: "$/.github/workflows/called.yml", result: "indeterminate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, remote := t.TempDir(), t.TempDir()
			workflow := "on: push\njobs:\n  test:\n"
			if strings.HasSuffix(test.uses, ".yml") {
				workflow += "    uses: " + test.uses + "\n"
			} else {
				workflow += "    runs-on: ubuntu-latest\n    steps:\n      - uses: " + test.uses + "\n"
			}
			path := filepath.Join(workspace, ".github", "workflows", "ci.yml")
			remotePath := filepath.Join(remote, ".github", "workflows", "ci.yml")
			calledPath := filepath.Join(remote, ".github", "workflows", "called.yml")
			for _, file := range []string{path, remotePath, calledPath, filepath.Join(remote, "action", "action.yml")} {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				data := workflow
				switch {
				case strings.HasSuffix(file, "action.yml"):
					data = "runs:\n  using: composite\n  steps:\n    - run: echo pinned\n      shell: bash\n"
				case file == calledPath:
					data = "on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: $/action\n"
				case file == remotePath && test.mismatch:
					data += "# different revision\n"
				}
				if err := os.WriteFile(file, []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			digest, err := actionsource.DigestTree(remote)
			if err != nil {
				t.Fatal(err)
			}
			source := &selfReferenceSource{root: remote, digest: digest}
			runtime := &profileValidationRuntime{actionSource: compiler.MemoizeRepositorySource(source), distributionDigest: "sha256:" + strings.Repeat("a", 64)}
			var stdout, stderr bytes.Buffer
			out := newProcessingOutput(t.Context(), "validate", "json", &stdout, &stderr, transport.Agent{})
			eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
			if test.event != "" {
				eventPath = ""
			}
			code := validateOne(t.Context(), out, path, eventPath, test.event, "hosted", "test", "", runtime, &stderr)
			var report compatibility.ProcessingReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Result != test.result || (code == 0) != (test.result == "admitted") {
				t.Fatalf("code=%d report=%s stderr=%s parse=%v", code, stdout.String(), stderr.String(), err)
			}
			if test.event != "" && len(source.refs) != 0 {
				t.Fatalf("synthetic event fetched source: %#v", source.refs)
			}
			if test.result == "admitted" && (len(source.refs) != 2 || !source.refs[0].RepositoryRoot || source.refs[0].Path != ".github/workflows/ci.yml" || source.refs[1].RepositoryRoot || source.refs[1].Path != "action") {
				t.Fatalf("source authorization = %#v", source.refs)
			}
		})
	}
}
