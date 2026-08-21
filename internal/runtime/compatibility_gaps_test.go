//go:build compatibility_gaps

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/compiler"
)

func TestCompatibilityGapOtherShells(t *testing.T) {
	bin := t.TempDir()
	pwsh := filepath.Join(bin, "pwsh")
	if err := os.WriteFile(pwsh, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runCompatibilityGapWorkflow(t, "other-shells.yml")
}

func TestCompatibilityGapCustomShellTemplates(t *testing.T) {
	for _, name := range []string{"Rscript", "julia"} {
		bin := t.TempDir()
		wrapper := filepath.Join(bin, name)
		if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nfor arg do case \"$arg\" in */buildkite-gha-shell-*) exec sh \"$arg\";; esac; done\nexit 2\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	runCompatibilityGapWorkflow(t, "custom-shell-templates.yml")
}

func runCompatibilityGapWorkflow(t *testing.T, name string) {
	t.Helper()
	root := filepath.Join("..", "..", "testdata", "compatibility-gaps")
	workflowPath := filepath.Join(".github", "workflows", name)
	source, err := os.ReadFile(filepath.Join(root, workflowPath))
	if err != nil {
		t.Fatal(err)
	}
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := compiler.CompileBundle(workflowPath, source, event, "compatibility-gap", "sha256:"+strings.Repeat("0", 64), "compatibility-gap")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(bundle.Plans))
	}
	result, err := (Runner{}).RunJob(t.Context(), bundle.Plans[0].Job, root)
	if err != nil {
		t.Fatalf("RunJob() conclusion = %q, error = %v", result.Conclusion, err)
	}
}
