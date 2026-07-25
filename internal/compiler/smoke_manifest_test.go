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

	wantOrder := []string{"smoke-shell", "smoke-concurrent", "smoke-ci", "smoke-artifact", "phase4-public-actions", "phase5-docker-action", "phase5-container-runtime", "unsupported-cache-service", "unsupported-job-container", "unsupported-service-container"}
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
		for _, path := range []string{fixture.Workflow, fixture.Event} {
			clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
			if !strings.HasPrefix(path, "testdata/") || filepath.IsAbs(path) || clean != path || slicesContain(strings.Split(path, "/"), "..") {
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
	for _, pattern := range []string{"testdata/smoke/.github/workflows/*.yml", "testdata/phase4/.github/workflows/*.yml", "testdata/phase5/.github/workflows/*.yml.tmpl", "testdata/phase5/runtime/.github/workflows/*.yml", "testdata/unsupported/.github/workflows/*.yml"} {
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

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
