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

	wantOrder := []string{"smoke-shell", "smoke-concurrent", "smoke-ci", "smoke-artifact", "smoke-artifact-multi-prefix", "example-basic", "example-artifacts", "example-advanced", "plugin-demo-cache", "public-actions", "dockerfile-action", "container-runtime", "summary-annotation", "workflow-command-annotations", "upload-artifact", "cache-v6", "cache-v5", "cache-v4", "unsupported-job-container", "unsupported-service-container"}
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
		if fixture.ActionPreflight != "none" && fixture.ActionPreflight != "hosted" {
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
	for _, pattern := range []string{".github/workflows/example-basic.yml", ".github/workflows/example-artifacts.yml", ".github/workflows/example-advanced.yml", "testdata/smoke/.github/workflows/*.yml", "testdata/plugin-demo/.github/workflows/cache.yml", "testdata/public-actions/.github/workflows/*.yml", "testdata/dockerfile-action/.github/workflows/*.yml.tmpl", "testdata/container-runtime/.github/workflows/*.yml", "testdata/unsupported/.github/workflows/*.yml"} {
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

func TestProductionPluginActionWorkflowCompilesDeterministically(t *testing.T) {
	root := filepath.Join("..", "..")
	workflowPath := filepath.Join(root, ".github", "workflows", "local-actions-oracle.yml")
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
