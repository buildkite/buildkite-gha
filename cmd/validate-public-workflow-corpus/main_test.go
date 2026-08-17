package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compatibility"
)

func TestParseSampleSize(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
		valid bool
	}{
		{value: "", valid: true},
		{value: "1", want: 1, valid: true},
		{value: "1000", want: 1000, valid: true},
		{value: "0"},
		{value: "01"},
		{value: "+1"},
		{value: "-1"},
		{value: "one"},
		{value: "1 "},
		{value: "999999999999999999999999999999999999"},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, err := parseSampleSize(test.value)
			if (err == nil) != test.valid {
				t.Fatalf("parseSampleSize(%q) error = %v, valid = %t", test.value, err, test.valid)
			}
			if got != test.want {
				t.Fatalf("parseSampleSize(%q) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestLatestWorkflowsSelectsNewestUID(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "workflows.csv.gz")
	writeWorkflowMetadata(t, metadata, [][]string{
		workflowMetadataHeader,
		{"one", "100", "True", "A", ".github/workflows/old.yml", "old-hash", "acme/one"},
		{"two", "150", "True", "A", ".github/workflows/two.yml", "two-hash", "acme/two"},
		{"one", "200", "True", "M", ".github/workflows/new.yml", "new-hash", "acme/one"},
		{"one", "200", "True", "M", ".github/workflows/tie.yml", "tie-hash", "acme/one"},
	})

	latest, err := latestWorkflows(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest workflow count = %d, want 2", len(latest))
	}
	if got := latest["one"]; got.committed != 200 || got.path != ".github/workflows/new.yml" || got.hash != "new-hash" {
		t.Fatalf("latest workflow for uid one = %#v", got)
	}
}

func TestExtractCorpusColdPathAndRecovery(t *testing.T) {
	corpusDir := t.TempDir()
	metadata := [][]string{
		workflowMetadataHeader,
		{"repo-b", "100", "True", "A", ".github/workflows/old.yml", "old-hash", "acme/b"},
		{"repo-b", "200", "True", "M", ".github/workflows/b.yml", "shared-hash", "acme/b"},
		{"repo-a", "100", "True", "A", ".github/workflows/a.yml", "shared-hash", "acme/a"},
		{"invalid", "100", "False", "A", ".github/workflows/invalid.yml", "invalid-hash", "acme/invalid"},
		{"deleted", "100", "True", "D", ".github/workflows/deleted.yml", "deleted-hash", "acme/deleted"},
		{"became-deleted", "100", "True", "A", ".github/workflows/removed.yml", "removed-hash", "acme/removed"},
		{"became-deleted", "200", "True", "D", ".github/workflows/removed.yml", "removed-hash", "acme/removed"},
	}
	writeWorkflowMetadata(t, filepath.Join(corpusDir, "workflows.csv.gz"), metadata)
	writeWorkflowArchive(t, filepath.Join(corpusDir, "workflows.tar.gz"), []archiveEntry{
		{name: "archive/shared-hash", contents: "name: shared\n"},
		{name: "archive/ignored-hash", contents: "name: ignored\n"},
	})

	var stdout bytes.Buffer
	if err := extractCorpus(corpusDir, &stdout); err != nil {
		t.Fatal(err)
	}
	if want := "2 live workflow files across 2 repos\nextracted 2\n"; stdout.String() != want {
		t.Fatalf("extraction output = %q, want %q", stdout.String(), want)
	}
	wantTSV := strings.Join([]string{
		"000000\tacme/a\t.github/workflows/a.yml\tshared-hash",
		"000001\tacme/b\t.github/workflows/b.yml\tshared-hash",
		"",
	}, "\n")
	assertFileContents(t, filepath.Join(corpusDir, "workflows.tsv"), wantTSV)
	for _, workflow := range []struct {
		id, filename string
	}{{"000000", "a.yml"}, {"000001", "b.yml"}} {
		assertFileContents(t, filepath.Join(corpusDir, "files", "000", workflow.id, ".github", "workflows", workflow.filename), "name: shared\n")
	}
	marker := filepath.Join(corpusDir, "files", ".complete-v3")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("completion marker: %v", err)
	}

	manifestPath, err := ensureManifest(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	wantRecords := []manifestRecord{
		{Hash: "shared-hash", ID: "000000", Path: ".github/workflows/a.yml", Repository: "acme/a", Source: filepath.Join(corpusDir, "files", "000", "000000", ".github", "workflows", "a.yml")},
		{Hash: "shared-hash", ID: "000001", Path: ".github/workflows/b.yml", Repository: "acme/b", Source: filepath.Join(corpusDir, "files", "000", "000001", ".github", "workflows", "b.yml")},
	}
	if !reflect.DeepEqual(records, wantRecords) {
		t.Fatalf("manifest records = %#v, want %#v", records, wantRecords)
	}

	if err := os.Remove(filepath.Join(corpusDir, "workflows.tar.gz")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := extractCorpus(corpusDir, &stdout); err != nil {
		t.Fatalf("reuse completed extraction: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("completed extraction output = %q, want none", stdout.String())
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "files", "stale"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(corpusDir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "reports", "stale"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWorkflowArchive(t, filepath.Join(corpusDir, "workflows.tar.gz"), []archiveEntry{{name: "shared-hash", contents: "name: recovered\n"}})
	stdout.Reset()
	if err := extractCorpus(corpusDir, &stdout); err != nil {
		t.Fatalf("recover incomplete extraction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(corpusDir, "reports")); !os.IsNotExist(err) {
		t.Fatalf("stale reports still exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(corpusDir, "files", "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale extracted file still exists: %v", err)
	}
	assertFileContents(t, filepath.Join(corpusDir, "files", "000", "000000", ".github", "workflows", "a.yml"), "name: recovered\n")
}

func TestExtractCorpusRejectsMissingWorkflow(t *testing.T) {
	corpusDir := t.TempDir()
	writeWorkflowMetadata(t, filepath.Join(corpusDir, "workflows.csv.gz"), [][]string{
		workflowMetadataHeader,
		{"one", "100", "True", "A", ".github/workflows/ci.yml", "wanted-hash", "acme/one"},
	})
	writeWorkflowArchive(t, filepath.Join(corpusDir, "workflows.tar.gz"), []archiveEntry{{name: "other-hash", contents: "name: other\n"}})

	err := extractCorpus(corpusDir, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "corpus archive is missing 1 selected workflows") {
		t.Fatalf("extractCorpus error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(corpusDir, "files", ".complete-v3")); !os.IsNotExist(err) {
		t.Fatalf("completion marker exists after failed extraction: %v", err)
	}
}

func TestExtractWorkflowFilesRejectsInvalidArchives(t *testing.T) {
	t.Run("malformed gzip", func(t *testing.T) {
		corpusDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(corpusDir, "workflows.tar.gz"), []byte("not gzip"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := extractWorkflowFiles(corpusDir, map[string][]workflowDestination{}); err == nil || !strings.Contains(err.Error(), "open workflow archive gzip stream") {
			t.Fatalf("extractWorkflowFiles error = %v", err)
		}
	})

	t.Run("truncated tar entry", func(t *testing.T) {
		corpusDir := t.TempDir()
		writeTruncatedWorkflowArchive(t, filepath.Join(corpusDir, "workflows.tar.gz"), "wanted-hash")
		wanted := map[string][]workflowDestination{
			"wanted-hash": {{id: "000000", filename: "ci.yml"}},
		}
		if _, err := extractWorkflowFiles(corpusDir, wanted); err == nil || !strings.Contains(err.Error(), "read archived workflow wanted-hash") {
			t.Fatalf("extractWorkflowFiles error = %v", err)
		}
	})
}

func TestSampleManifestStable(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "workflows.jsonl")
	records := []manifestRecord{
		{Hash: "hash-one", ID: "000000", Path: ".github/workflows/ci.yml", Repository: "acme/one", Source: "/workflows/one.yml"},
		{Hash: "hash-two", ID: "000001", Path: ".github/workflows/build.yaml", Repository: "acme/two", Source: "/workflows/two.yml"},
	}
	if err := writeJSONLines(source, records); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	key, manifest, err := sampleManifest(source, filepath.Join(root, "samples"), 1, "testing", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := "n1-scf80cd8aed48-m3c8f85957d35"; key != want {
		t.Fatalf("sample key = %q, want %q", key, want)
	}
	selected, err := loadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0] != records[0] {
		t.Fatalf("selected records = %#v, want %#v", selected, records[:1])
	}
	if want := "sample: 1 workflows, seed 'testing', key " + key + "\n"; stderr.String() != want {
		t.Fatalf("sample summary = %q, want %q", stderr.String(), want)
	}
}

func TestTallyReports(t *testing.T) {
	root := t.TempDir()
	workflow := filepath.Join(root, "workflow.yml")
	contextWorkflow := filepath.Join(root, "context-workflow.yml")
	manifest := filepath.Join(root, "workflows.jsonl")
	if err := writeJSONLines(manifest, []manifestRecord{
		{Hash: "hash", ID: "000000", Path: ".github/workflows/ci.yml", Repository: "acme/one", Source: workflow},
		{Hash: "context-hash", ID: "000001", Path: ".github/workflows/pr.yml", Repository: "acme/two", Source: contextWorkflow},
	}); err != nil {
		t.Fatal(err)
	}
	reports := filepath.Join(root, "reports")
	if err := os.Mkdir(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	validation := compatibility.ProcessingReport{Schema: compatibility.ProcessingSchema}
	evaluation := compatibility.ProcessingReport{
		Schema:      compatibility.ProcessingSchema,
		Diagnostics: []compatibility.Diagnostic{{Code: "E_EXAMPLE"}},
	}
	report := compatibility.ProcessingReportV3{
		Schema: compatibility.ProcessingSchemaV3, Workflow: workflow, Profile: "hosted", Result: "admitted",
		Validation:  validation,
		Evaluations: []compatibility.EventEvaluation{{Event: "push", Source: "generated", Report: evaluation}},
	}
	contents, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "result.json"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	contextReport := compatibility.ProcessingReportV3{
		Schema: compatibility.ProcessingSchemaV3, Workflow: contextWorkflow, Profile: "hosted", Result: "context-required",
		Validation: validation, Evaluations: []compatibility.EventEvaluation{},
	}
	contents, err = json.Marshal(contextReport)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reports, "context-result.json"), contents, 0o644); err != nil {
		t.Fatal(err)
	}

	tallyPath := filepath.Join(root, "tally.json")
	var stdout bytes.Buffer
	validator := validatorInfo{commit: "commit", version: "version", digest: "digest"}
	if err := tallyReports(manifest, reports, tallyPath, validator, "full", "default", 0, &stdout); err != nil {
		t.Fatal(err)
	}
	var tally tallyOutput
	tallyContents, err := os.ReadFile(tallyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(tallyContents, &tally); err != nil {
		t.Fatal(err)
	}
	if tally.Repos != 2 || tally.MeasuredRepos != 1 || tally.CompatibleRepos != 1 || tally.ContextRequiredRepos != 1 || tally.UnparseableReports != 0 {
		t.Fatalf("repository tally = %#v", tally)
	}
	if tally.ByFinding["E_EXAMPLE"] != 1 || tally.ByRepo["E_EXAMPLE"] != 1 || tally.Evaluations["push"] != 1 {
		t.Fatalf("diagnostic tally = %#v", tally)
	}
	if tally.WorkflowResults["admitted"] != 1 || tally.WorkflowResults["context-required"] != 1 {
		t.Fatalf("workflow result tally = %#v", tally.WorkflowResults)
	}
	if !strings.Contains(stdout.String(), "measured: 1   compatible: 1 (100.00% of measured)") {
		t.Fatalf("summary did not contain repository tally:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "50.00%       1  E_EXAMPLE") {
		t.Fatalf("summary did not contain diagnostic tally:\n%s", stdout.String())
	}
}

var workflowMetadataHeader = []string{"uid", "committed_date", "valid_workflow", "git_change_type", "file_path", "file_hash", "repository"}

type archiveEntry struct {
	name, contents string
}

func writeWorkflowMetadata(t *testing.T, destination string, records [][]string) {
	t.Helper()
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	writer := csv.NewWriter(compressed)
	if err := writer.WriteAll(records); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeWorkflowArchive(t *testing.T, destination string, entries []archiveEntry) {
	t.Helper()
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o644, Size: int64(len(entry.contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTruncatedWorkflowArchive(t *testing.T, destination, hash string) {
	t.Helper()
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: hash, Mode: 0o644, Size: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("truncated")); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("%s contents = %q, want %q", path, contents, want)
	}
}
