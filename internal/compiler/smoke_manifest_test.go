package compiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scripts/smoke-local validates the manifest's shape and paths. This test
// keeps every checked-in fixture workflow classified in the manifest.
func TestSmokeManifestInventoriesEveryFixtureWorkflow(t *testing.T) {
	root := filepath.Join("..", "..")
	source, err := os.ReadFile(filepath.Join(root, "testdata", "smoke", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Fixtures []struct {
			Workflow string `json:"workflow"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		t.Fatal(err)
	}
	var inventoried []string
	for _, fixture := range manifest.Fixtures {
		inventoried = append(inventoried, fixture.Workflow)
	}
	slices.Sort(inventoried)

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
	slices.Sort(checkedIn)
	if !slices.Equal(inventoried, checkedIn) {
		t.Fatalf("inventory = %v, checked-in workflows = %v", inventoried, checkedIn)
	}
}
