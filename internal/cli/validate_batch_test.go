package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
)

type batchCountingActionSource struct {
	calls  int
	root   string
	digest string
}

func (s *batchCountingActionSource) Fetch(_ context.Context, ref actionsource.Reference) (actionsource.Resolved, actionsource.Materialized, error) {
	s.calls++
	commit := strings.Repeat("a", 40)
	return actionsource.Resolved{Reference: ref, Commit: commit, SourceDigest: s.digest}, actionsource.Materialized{RepositoryRoot: s.root, ActionRoot: s.root, SourceDigest: s.digest}, nil
}

func TestValidateBatchWritesAndResumesAtomicReports(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "reports")
	records := []batchValidationRecord{
		{ID: "one", Repository: "owner/one", Path: ".github/workflows/ci.yml", Hash: strings.Repeat("1", 64), Source: filepath.Join(root, "one.yml")},
		{ID: "two", Repository: "owner/two", Path: ".github/workflows/test.yml", Hash: strings.Repeat("2", 64), Source: filepath.Join(root, "two.yml")},
	}
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n")
	for _, record := range records {
		if err := os.WriteFile(record.Source, workflow, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := writeBatchManifest(t, root, records)
	args := []string{"validate-batch", "--manifest", manifest, "--output-dir", output, "--corpus-id", "test-corpus"}
	var stdout, stderr bytes.Buffer
	if code := Run(args, &stdout, &stderr, "dev"); code != 0 || stdout.Len() != 0 {
		t.Fatalf("Run() = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	reports := batchReports(t, output)
	if len(reports) != len(records) {
		t.Fatalf("reports = %v, want %d", reports, len(records))
	}
	for _, path := range reports {
		var report compatibility.ProcessingReportV3
		contents, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(contents, &report) != nil || report.Schema != compatibility.ProcessingSchemaV3 || report.Profile != hostedProfile {
			t.Fatalf("report %q is invalid: %v\n%s", path, err, contents)
		}
	}

	first := reports[0]
	if err := os.WriteFile(first, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := Run(args, &stdout, &stderr, "dev"); code != 0 || !strings.Contains(stderr.String(), "wrote 1 reports; resumed 1") {
		t.Fatalf("resumed Run() = %d, stderr %q", code, stderr.String())
	}
}

func TestBatchValidationReusesActionResolutionAcrossWorkflows(t *testing.T) {
	root := t.TempDir()
	actionRoot := filepath.Join(root, "action")
	if err := os.Mkdir(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "action.yml"), []byte("runs:\n  using: node24\n  main: index.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionRoot, "index.js"), []byte("console.log('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := actionsource.DigestTree(actionRoot)
	if err != nil {
		t.Fatal(err)
	}
	source := &batchCountingActionSource{root: actionRoot, digest: digest}
	_, _, distributionDigest, err := executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime := &profileValidationRuntime{actionSource: compiler.MemoizeActionSource(source), distributionDigest: distributionDigest}
	workflow := []byte("on: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/action@v1\n")
	for _, name := range []string{"one", "two"} {
		path := filepath.Join(root, name, ".github", "workflows", "ci.yml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, workflow, 0o600); err != nil {
			t.Fatal(err)
		}
		out := processingOutput{command: "validate-batch", format: "json", reports: io.Discard, stderr: io.Discard}
		if code := validateAllEvents(out, path, "dev", "", runtime, io.Discard); code != 0 {
			t.Fatalf("validateAllEvents(%s) = %d", name, code)
		}
	}
	if source.calls != 1 {
		t.Fatalf("action source calls = %d, want 1", source.calls)
	}
}

func TestBatchValidationManifestAndIdentity(t *testing.T) {
	root := t.TempDir()
	valid := batchValidationRecord{ID: "one", Repository: "owner/repo", Path: "ci.yml", Hash: "hash", Source: "source.yml"}
	manifest := writeBatchManifest(t, root, []batchValidationRecord{valid, valid})
	if _, err := loadBatchValidationManifest(manifest); err == nil || !strings.Contains(err.Error(), "repeats id") {
		t.Fatalf("duplicate manifest error = %v", err)
	}
	if _, err := parseBatchValidationArgs([]string{"--manifest", "manifest"}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing options error = %v", err)
	}
	options := batchValidationArgs{outputDir: root, corpusID: "record-one"}
	base := batchValidationResultPath(options, valid, "validator-one")
	variants := []string{
		batchValidationResultPath(batchValidationArgs{outputDir: root, corpusID: "record-two"}, valid, "validator-one"),
		batchValidationResultPath(options, valid, "validator-two"),
		batchValidationResultPath(options, batchValidationRecord{ID: "two", Repository: valid.Repository, Path: valid.Path, Hash: valid.Hash}, "validator-one"),
		batchValidationResultPath(options, batchValidationRecord{ID: valid.ID, Repository: valid.Repository, Path: valid.Path, Hash: "other"}, "validator-one"),
	}
	for _, variant := range variants {
		if variant == base {
			t.Fatalf("result identity collision: %q", variant)
		}
	}
}

func writeBatchManifest(t *testing.T, root string, records []batchValidationRecord) string {
	t.Helper()
	path := filepath.Join(root, "manifest.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func batchReports(t *testing.T, root string) []string {
	t.Helper()
	var reports []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".json") {
			reports = append(reports, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return reports
}
