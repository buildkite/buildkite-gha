package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunJobPythonShellUsesTemporaryScript(t *testing.T) {
	installPythonShellTestCommand(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{
		ID:    "python",
		Kind:  "run",
		Shell: "python",
		Command: `import os
from pathlib import Path

assert Path.cwd() == Path(os.environ["GITHUB_WORKSPACE"])
assert Path(__file__).suffix == ".py"
with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
    output.write(f"script={__file__}\n")
`,
	}})
	job.Outputs = map[string]string{"script": "${{ steps.python.outputs.script }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	script := result.Outputs["script"]
	if !strings.HasSuffix(script, ".py") {
		t.Fatalf("Python script path = %q, want .py suffix", script)
	}
	if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary Python script remains at %q: %v", script, err)
	}
}

func TestRunJobCustomShellTemplatesUseTemporaryScripts(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  string
		shell    string
		wantArgs []string
	}{
		{name: "R", command: "Rscript", shell: `Rscript --vanilla "two words" {0} --after='quoted value'`, wantArgs: []string{"--vanilla", "two words"}},
		{name: "Julia", command: "julia", shell: `julia --color=yes {0} --project "two words"`, wantArgs: []string{"--color=yes"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := installCustomShellTestCommand(t, test.command)
			workspace := t.TempDir()
			arguments := filepath.Join(workspace, "arguments")
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{
				{ID: "path", Kind: "run", Shell: "sh", Command: `printf '%s\n' "$CUSTOM_SHELL_BIN" >> "$GITHUB_PATH"`, Env: map[string]string{"CUSTOM_SHELL_BIN": bin}},
				{
					ID:      "custom",
					Kind:    "run",
					Shell:   test.shell,
					Env:     map[string]string{"CUSTOM_SHELL_ARGS": arguments},
					Command: `printf 'script=%s\n' "$0" >> "$GITHUB_OUTPUT"`,
				},
			})
			job.Outputs = map[string]string{"script": "${{ steps.custom.outputs.script }}"}
			result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
			if err != nil {
				t.Fatalf("RunJob() error = %v", err)
			}
			script := result.Outputs["script"]
			if base := filepath.Base(script); !strings.HasPrefix(base, "buildkite-gha-shell-") || filepath.Ext(base) != "" {
				t.Fatalf("custom shell script path = %q, want extensionless temporary script", script)
			}
			if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary custom shell script remains at %q: %v", script, err)
			}
			data, err := os.ReadFile(arguments)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(got) < len(test.wantArgs)+1 || !slices.Equal(got[:len(test.wantArgs)], test.wantArgs) || got[len(test.wantArgs)] != script {
				t.Fatalf("custom shell arguments = %#v, want prefix %#v followed by %q", got, test.wantArgs, script)
			}
		})
	}
}

func TestRunJobLoginBashTemplate(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "login", Kind: "run", Shell: "bash -l {0}", Command: `printf 'value=login-bash\n' >> "$GITHUB_OUTPUT"`}})
	job.Outputs = map[string]string{"value": "${{ steps.login.outputs.value }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Outputs["value"] != "login-bash" {
		t.Fatalf("RunJob() output = %q, error = %v, want login-bash", result.Outputs["value"], err)
	}
}

func TestCompositePythonShell(t *testing.T) {
	installPythonShellTestCommand(t)
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Python composite
outputs:
  value:
    value: ${{ steps.python.outputs.value }}
runs:
  using: composite
  steps:
    - id: python
      shell: python
      run: |
        import os
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
            output.write("value=python-composite\n")
`)
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	job.Outputs = map[string]string{"value": "${{ steps.composite.outputs.value }}"}
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil {
		t.Fatalf("RunJob() error = %v", err)
	}
	if result.Outputs["value"] != "python-composite" {
		t.Fatalf("RunJob() output = %q, want python-composite", result.Outputs["value"])
	}
}

func installCustomShellTestCommand(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	wrapper := `#!/bin/sh
: > "$CUSTOM_SHELL_ARGS"
script=
for arg do
  printf '%s\n' "$arg" >> "$CUSTOM_SHELL_ARGS"
  case "$arg" in
    */buildkite-gha-shell-*) script=$arg ;;
  esac
done
test -n "$script"
exec /bin/sh "$script"
`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installPythonShellTestCommand(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	python := filepath.Join(dir, "python")
	wrapper := "#!/bin/sh\nBUILDKITE_GHA_TEST_PYTHON_HELPER=1 exec " + shellTestQuote(executable) + " -test.run '^TestPythonShellCommandHelper$' -- \"$@\"\n"
	if err := os.WriteFile(python, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPythonShellCommandHelper(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_TEST_PYTHON_HELPER") == "" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatalf("Python helper arguments = %#v", os.Args)
	}
	script := os.Args[separator+1]
	source, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(script) != ".py" {
		t.Fatalf("Python script = %q, want .py suffix", script)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != os.Getenv("GITHUB_WORKSPACE") {
		t.Fatalf("working directory = %q, %v; workspace = %q", cwd, err, os.Getenv("GITHUB_WORKSPACE"))
	}
	var output string
	switch {
	case bytes.Contains(source, []byte(`script={__file__}`)):
		output = "script=" + script + "\n"
	case bytes.Contains(source, []byte(`value=python-composite`)):
		output = "value=python-composite\n"
	default:
		t.Fatalf("unexpected Python source: %s", source)
	}
	file, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(output); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompositeStepWorkingDirectoryEvaluatesExpressions(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", `name: Working-directory composite
inputs:
  dir:
    required: false
runs:
  using: composite
  steps:
    - shell: bash
      working-directory: ${{ inputs.dir || '.' }}
      run: test "$(basename "$PWD")" = sub
    - shell: bash
      working-directory: literal-dir
      run: test "$(basename "$PWD")" = literal-dir
`)
	for _, directory := range []string{"sub", "literal-dir"} {
		if err := os.Mkdir(filepath.Join(workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite", With: map[string]string{"dir": "sub"}}})
	result, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
}

func TestShellStepsMissingWorkingDirectoryFailClearly(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps []runtimeTestStep
		setup func(*testing.T, string)
	}{
		{
			name:  "workflow",
			steps: []runtimeTestStep{{ID: "run", Kind: "run", Shell: "sh", Command: "true", WorkingDirectory: "missing"}},
		},
		{
			name:  "composite",
			steps: []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}},
			setup: func(t *testing.T, workspace string) {
				t.Helper()
				writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "name: composite\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      working-directory: missing\n      run: true\n")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			workflowPath := ".github/workflows/test.yml"
			writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
			if test.setup != nil {
				test.setup(t, workspace)
			}
			job := runtimePlan(t, workspace, workflowPath, test.steps)
			if _, err := (Runner{}).runTestJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), `working directory "missing" does not exist`) {
				t.Fatalf("RunJob() error = %v, want missing working directory diagnostic", err)
			}
		})
	}
}

func TestCompositeShellWorkingDirectoryFailurePrecedesShellEvaluation(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: runtime test\n")
	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "name: composite\nruns:\n  using: composite\n  steps:\n    - shell: ${{ fromJSON('invalid') }}\n      working-directory: missing\n      run: true\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	_, err := (Runner{}).runTestJob(t.Context(), job, workspace)
	if err == nil || !strings.Contains(err.Error(), `working directory "missing" does not exist`) || strings.Contains(err.Error(), "fromJSON") {
		t.Fatalf("RunJob() error = %v, want only the prior working-directory failure", err)
	}
}
