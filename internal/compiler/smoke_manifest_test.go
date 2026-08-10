package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestSmokeManifestInventory(t *testing.T) {
	type fixture struct {
		ID              string `json:"id"`
		Workflow        string `json:"workflow"`
		Event           string `json:"event"`
		Expectation     string `json:"expectation"`
		Evidence        string `json:"evidence"`
		Note            string `json:"note"`
		ActionPreflight string `json:"action_preflight"`
	}
	var manifest struct {
		Schema   string    `json:"schema"`
		Fixtures []fixture `json:"fixtures"`
	}
	root := filepath.Join("..", "..")
	source, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing manifest value: %v", err)
	}
	if manifest.Schema != "buildkite-gha/smoke-manifest/v1" {
		t.Fatalf("schema = %q", manifest.Schema)
	}

	wantOrder := []string{"smoke-shell", "smoke-concurrent", "smoke-ci", "smoke-artifact", "migration-poc-basic", "migration-poc-artifacts", "migration-poc-advanced", "migration-poc-cache", "phase4-public-actions", "phase5-docker-action", "phase5-container-runtime", "phase6-summary-annotation", "phase6-workflow-command-annotations", "phase6-upload-artifact", "phase6-cache-v6", "unsupported-job-container", "unsupported-service-container"}
	if len(manifest.Fixtures) != len(wantOrder) {
		t.Fatalf("fixtures = %d, want %d", len(manifest.Fixtures), len(wantOrder))
	}
	idPattern := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	ids, pairs, inventoried := map[string]bool{}, map[string]bool{}, []string{}
	for i, fixture := range manifest.Fixtures {
		if fixture.ID != wantOrder[i] {
			t.Fatalf("fixture %d ID = %q, want %q", i, fixture.ID, wantOrder[i])
		}
		if !idPattern.MatchString(fixture.ID) || ids[fixture.ID] {
			t.Fatalf("invalid or duplicate ID %q", fixture.ID)
		}
		ids[fixture.ID] = true
		if fixture.Expectation != "compile-pass" && fixture.Expectation != "compile-unsupported" && fixture.Expectation != "runtime-pass" && fixture.Expectation != "runtime-unsupported" && fixture.Expectation != "future" {
			t.Fatalf("%s: invalid expectation %q", fixture.ID, fixture.Expectation)
		}
		if fixture.ActionPreflight != "none" && fixture.ActionPreflight != "hosted-tokenless" {
			t.Fatalf("%s: invalid action preflight %q", fixture.ID, fixture.ActionPreflight)
		}
		if strings.TrimSpace(fixture.Evidence) == "" || strings.TrimSpace(fixture.Note) == "" {
			t.Fatalf("%s: evidence and note are required", fixture.ID)
		}
		for index, path := range []string{fixture.Workflow, fixture.Event} {
			clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
			allowedRootWorkflow := index == 0 && (path == ".github/workflows/example-basic.yml" || path == ".github/workflows/example-artifacts.yml" || path == ".github/workflows/example-advanced.yml")
			if (!strings.HasPrefix(path, "testdata/") && !allowedRootWorkflow) || filepath.IsAbs(path) || clean != path || slicesContain(strings.Split(path, "/"), "..") {
				t.Fatalf("%s: unsafe path %q", fixture.ID, path)
			}
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("%s: missing regular file %q: %v", fixture.ID, path, err)
			}
		}
		pair := fixture.Workflow + "\x00" + fixture.Event
		if pairs[pair] {
			t.Fatalf("duplicate workflow/event pair for %s", fixture.ID)
		}
		pairs[pair] = true
		inventoried = append(inventoried, fixture.Workflow)
	}

	var checkedIn []string
	for _, pattern := range []string{".github/workflows/example-basic.yml", ".github/workflows/example-artifacts.yml", ".github/workflows/example-advanced.yml", "testdata/smoke/.github/workflows/*.yml", "testdata/poc/.github/workflows/cache.yml", "testdata/phase4/.github/workflows/*.yml", "testdata/phase5/.github/workflows/*.yml.tmpl", "testdata/phase5/runtime/.github/workflows/*.yml", "testdata/phase6/.github/workflows/*.yml", "testdata/unsupported/.github/workflows/*.yml"} {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			checkedIn = append(checkedIn, filepath.ToSlash(strings.TrimPrefix(match, root+string(filepath.Separator))))
		}
	}
	sort.Strings(checkedIn)
	sortedInventory := append([]string(nil), inventoried...)
	sort.Strings(sortedInventory)
	if fmt.Sprint(sortedInventory) != fmt.Sprint(checkedIn) {
		t.Fatalf("inventory = %v, checked-in workflows = %v", sortedInventory, checkedIn)
	}
}

func TestOSSCorpusManifest(t *testing.T) {
	type expectation struct {
		Compile           string  `json:"compile"`
		Profile           string  `json:"profile"`
		LogicalJobs       *int    `json:"logical_jobs"`
		Instances         *int    `json:"instances"`
		DiagnosticCode    *string `json:"diagnostic_code"`
		DiagnosticPattern *string `json:"diagnostic_pattern"`
		WarningCode       *string `json:"warning_code"`
	}
	type corpusCase struct {
		ID            string      `json:"id"`
		Repository    string      `json:"repository"`
		Commit        string      `json:"commit"`
		Workflow      string      `json:"workflow"`
		DefaultBranch string      `json:"default_branch"`
		Event         string      `json:"event"`
		GitHubRun     *string     `json:"github_run"`
		Expected      expectation `json:"expected"`
		Runtime       string      `json:"runtime"`
		Note          string      `json:"note"`
	}
	var manifest struct {
		Schema string       `json:"schema"`
		Cases  []corpusCase `json:"cases"`
	}
	root := filepath.Join("..", "..")
	source, err := os.ReadFile(filepath.Join(root, "testdata", "oss", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing manifest value: %v", err)
	}
	if manifest.Schema != "buildkite-gha/oss-corpus/v1" {
		t.Fatalf("schema = %q", manifest.Schema)
	}
	wantOrder := []string{"urfave-cli-lint", "fastify-markdown-lint", "prettier-lint", "bat-changelog", "p-map-main", "fzf-linux", "jq-valgrind", "go-task-ci", "gum-build", "just-ci"}
	if len(manifest.Cases) != len(wantOrder) {
		t.Fatalf("cases = %d, want %d", len(manifest.Cases), len(wantOrder))
	}
	idPattern := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	repositoryPattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	commitPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)
	workflowPattern := regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`)
	branchPattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	runPattern := regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/actions/runs/[1-9][0-9]*$`)
	ids, sources := map[string]bool{}, map[string]bool{}
	for i, test := range manifest.Cases {
		if test.ID != wantOrder[i] {
			t.Fatalf("case %d ID = %q, want %q", i, test.ID, wantOrder[i])
		}
		if !idPattern.MatchString(test.ID) || ids[test.ID] {
			t.Fatalf("invalid or duplicate ID %q", test.ID)
		}
		ids[test.ID] = true
		if !repositoryPattern.MatchString(test.Repository) || !commitPattern.MatchString(test.Commit) || !workflowPattern.MatchString(test.Workflow) || !branchPattern.MatchString(test.DefaultBranch) || strings.Contains(test.Workflow, "..") || strings.Contains(test.DefaultBranch, "..") {
			t.Fatalf("%s: unsafe source identity", test.ID)
		}
		if test.GitHubRun != nil && (!runPattern.MatchString(*test.GitHubRun) || !strings.HasPrefix(*test.GitHubRun, "https://github.com/"+test.Repository+"/actions/runs/")) {
			t.Fatalf("%s: invalid GitHub run %q", test.ID, *test.GitHubRun)
		}
		sourceID := test.Repository + "\x00" + test.Commit + "\x00" + test.Workflow
		if sources[sourceID] {
			t.Fatalf("%s: duplicate source identity", test.ID)
		}
		sources[sourceID] = true
		if test.Event != "push" && test.Event != "pull_request" {
			t.Fatalf("%s: event = %q", test.ID, test.Event)
		}
		if test.Expected.Compile != "compilable" && test.Expected.Compile != "incompatible" {
			t.Fatalf("%s: compile expectation = %q", test.ID, test.Expected.Compile)
		}
		if test.Expected.Profile != "admitted" && test.Expected.Profile != "not-admitted" && test.Expected.Profile != "indeterminate" && test.Expected.Profile != "not-run" {
			t.Fatalf("%s: profile expectation = %q", test.ID, test.Expected.Profile)
		}
		if test.Expected.Compile == "incompatible" {
			if test.Expected.Profile != "not-run" || test.Expected.LogicalJobs != nil || test.Expected.Instances != nil || test.Expected.DiagnosticCode == nil || *test.Expected.DiagnosticCode != "E_COMPILE" || test.Expected.DiagnosticPattern == nil || strings.TrimSpace(*test.Expected.DiagnosticPattern) == "" {
				t.Fatalf("%s: incompatible case has inconsistent profile expectation", test.ID)
			}
		} else if test.Expected.Profile == "admitted" {
			if test.Expected.LogicalJobs == nil || *test.Expected.LogicalJobs <= 0 || test.Expected.Instances == nil || *test.Expected.Instances <= 0 || test.Expected.DiagnosticCode != nil || test.Expected.DiagnosticPattern != nil {
				t.Fatalf("%s: admitted case has a diagnostic expectation", test.ID)
			}
		} else if test.Expected.Profile == "not-run" || test.Expected.LogicalJobs == nil || *test.Expected.LogicalJobs <= 0 || test.Expected.Instances == nil || *test.Expected.Instances <= 0 || test.Expected.DiagnosticCode == nil || test.Expected.DiagnosticPattern == nil || strings.TrimSpace(*test.Expected.DiagnosticPattern) == "" {
			t.Fatalf("%s: compilable case has incomplete profile expectation", test.ID)
		}
		if test.Expected.DiagnosticPattern != nil {
			if _, err := regexp.Compile(*test.Expected.DiagnosticPattern); err != nil {
				t.Fatalf("%s: invalid diagnostic pattern: %v", test.ID, err)
			}
		}
		if test.Expected.WarningCode != nil && !regexp.MustCompile(`^W_[A-Z0-9_]+$`).MatchString(*test.Expected.WarningCode) {
			t.Fatalf("%s: invalid warning code %q", test.ID, *test.Expected.WarningCode)
		}
		if test.Runtime != "candidate-after-admission" && test.Runtime != "deferred" && test.Runtime != "unsupported" {
			t.Fatalf("%s: runtime classification = %q", test.ID, test.Runtime)
		}
		if strings.TrimSpace(test.Note) == "" {
			t.Fatalf("%s: note is required", test.ID)
		}
	}
}

func TestProductionPluginActionWorkflowCompilesDeterministically(t *testing.T) {
	root := filepath.Join("..", "..")
	workflowPath := filepath.Join(root, ".github", "workflows", "phase-4-actions-oracle.yml")
	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := Compile(workflowPath, workflow, event)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(workflowPath, workflow, event)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || !bytes.Equal(first, second) {
		t.Fatal("production plugin action workflow compilation is empty or nondeterministic")
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
