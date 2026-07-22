package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAndClassify(t *testing.T) {
	tests := []struct {
		name         string
		using        string
		wantRuntime  Runtime
		wantCapacity string
		wantError    string
	}{
		{name: "Node 24", using: "node24", wantRuntime: RuntimeNode24},
		{name: "composite", using: "composite", wantRuntime: RuntimeComposite},
		{name: "Docker", using: "docker", wantRuntime: RuntimeDocker, wantCapacity: "docker"},
		{name: "Node 20", using: "node20", wantError: `unsupported runtime "node20"`},
		{name: "unknown", using: "future", wantError: `unsupported runtime "future"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeAction(t, root, "action.yml", "runs:\n  using: "+test.using+"\n")
			action, err := Load(root, ".")
			if err != nil {
				t.Fatal(err)
			}
			gotRuntime, err := action.Runtime()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Runtime() error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || gotRuntime != test.wantRuntime {
				t.Fatalf("Runtime() = %q, %v, want %q, nil", gotRuntime, err, test.wantRuntime)
			}
			capabilities := gotRuntime.RequiredCapabilities()
			if test.wantCapacity == "" && len(capabilities) != 0 {
				t.Fatalf("RequiredCapabilities() = %#v, want none", capabilities)
			}
			if test.wantCapacity != "" && (len(capabilities) != 1 || capabilities[0] != test.wantCapacity) {
				t.Fatalf("RequiredCapabilities() = %#v, want [%s]", capabilities, test.wantCapacity)
			}
		})
	}
}

func TestLoadIsStrictAndConfined(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "unexpected: true\nruns:\n  using: node24\n")
		if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
			t.Fatalf("Load() error = %v, want unknown field rejection", err)
		}
	})

	t.Run("multiple documents", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "runs:\n  using: node24\n---\nruns:\n  using: docker\n")
		if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
			t.Fatalf("Load() error = %v, want multiple document rejection", err)
		}
	})

	t.Run("action symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		writeAction(t, outside, "action.yml", "runs:\n  using: node24\n")
		if err := os.Symlink(outside, filepath.Join(root, "escaped")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root, "escaped"); err == nil || !strings.Contains(err.Error(), "escapes root") {
			t.Fatalf("Load() error = %v, want action symlink rejection", err)
		}
	})

	t.Run("metadata symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		writeAction(t, outside, "action.yml", "runs:\n  using: node24\n")
		if err := os.Mkdir(filepath.Join(root, "action"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "action.yml"), filepath.Join(root, "action", "action.yml")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root, "action"); err == nil || !strings.Contains(err.Error(), "escapes root") {
			t.Fatalf("Load() error = %v, want metadata symlink rejection", err)
		}
	})
}

func TestLoadNormalizesNamesAndRejectsCollisions(t *testing.T) {
	root := t.TempDir()
	writeAction(t, root, "action.yml", `inputs:
  Message:
    required: true
outputs:
  Result:
    value: first
runs:
  using: composite
  steps: []
`)
	action, err := Load(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := action.Inputs["message"]; !ok {
		t.Fatalf("Load() inputs = %#v, want lower-case name", action.Inputs)
	}
	if _, ok := action.Outputs["result"]; !ok {
		t.Fatalf("Load() outputs = %#v, want lower-case name", action.Outputs)
	}

	writeAction(t, root, "action.yml", `outputs:
  Result:
    value: first
  result:
    value: second
runs:
  using: composite
  steps: []
`)
	if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), `duplicate case-insensitive name "result"`) {
		t.Fatalf("Load() error = %v, want output collision rejection", err)
	}
}

func writeAction(t *testing.T, root, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
