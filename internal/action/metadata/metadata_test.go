package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		{name: "Node 16", using: "node16", wantRuntime: RuntimeNode16},
		{name: "Node 20 declaration", using: "node20", wantRuntime: RuntimeNode24},
		{name: "Node 24", using: "node24", wantRuntime: RuntimeNode24},
		{name: "composite", using: "composite", wantRuntime: RuntimeComposite},
		{name: "Docker", using: "docker", wantRuntime: RuntimeDocker, wantCapacity: "docker,network"},
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
			if test.wantCapacity != "" && strings.Join(capabilities, ",") != test.wantCapacity {
				t.Fatalf("RequiredCapabilities() = %#v, want %s", capabilities, test.wantCapacity)
			}
		})
	}
}

func TestValidateDockerEntrypoints(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Metadata)
		setup  func(string)
		ok     bool
	}{
		{name: "exact Dockerfile", ok: true},
		{name: "non Dockerfile image", mutate: func(m *Metadata) { m.Runs.Image = "docker://alpine" }},
		{name: "entrypoint", mutate: func(m *Metadata) { m.Runs.Entrypoint = "main.sh" }},
		{name: "args", mutate: func(m *Metadata) { m.Runs.Args = []string{"", " --flag "} }, ok: true},
		{name: "main", mutate: func(m *Metadata) { m.Runs.Main = "main.sh" }},
		{name: "pre", mutate: func(m *Metadata) { m.Runs.Pre = "pre.sh" }},
		{name: "pre condition", mutate: func(m *Metadata) { m.Runs.PreIf = "always()" }},
		{name: "pre entrypoint", mutate: func(m *Metadata) { m.Runs.PreEntrypoint = "pre.sh" }},
		{name: "post", mutate: func(m *Metadata) { m.Runs.Post = "post.sh" }},
		{name: "post condition", mutate: func(m *Metadata) { m.Runs.PostIf = "always()" }},
		{name: "post entrypoint", mutate: func(m *Metadata) { m.Runs.PostEntrypoint = "post.sh" }},
		{name: "missing Dockerfile", setup: func(dir string) { _ = os.Remove(filepath.Join(dir, "Dockerfile")) }},
		{name: "directory Dockerfile", setup: func(dir string) {
			_ = os.Remove(filepath.Join(dir, "Dockerfile"))
			_ = os.Mkdir(filepath.Join(dir, "Dockerfile"), 0o700)
		}},
		{name: "symlink Dockerfile", setup: func(dir string) {
			_ = os.Remove(filepath.Join(dir, "Dockerfile"))
			_ = os.Symlink("target", filepath.Join(dir, "Dockerfile"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			data, _ := os.ReadFile(filepath.Join(base, "Dockerfile"))
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			m := Metadata{Path: dir, Runs: Runs{Using: "docker", Image: "Dockerfile"}}
			if test.mutate != nil {
				test.mutate(&m)
			}
			if test.setup != nil {
				test.setup(dir)
			}
			runtime, err := m.Runtime()
			if err == nil {
				err = m.ValidateEntrypoints(runtime)
			}
			if (err == nil) != test.ok {
				t.Fatalf("validation error = %v, want success %v", err, test.ok)
			}
		})
	}
}

func TestLoadDockerArgsRequiresStringSequence(t *testing.T) {
	for _, test := range []struct {
		name string
		args string
		ok   bool
	}{
		{name: "ordered strings", args: "[first, '', '  ', '--privileged']", ok: true},
		{name: "empty sequence", args: "[]", ok: true},
		{name: "scalar", args: "value"},
		{name: "number", args: "[1]"},
		{name: "mapping", args: "{name: value}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeAction(t, root, "Dockerfile", "FROM scratch\n")
			writeAction(t, root, "action.yml", "runs:\n  using: docker\n  image: Dockerfile\n  args: "+test.args+"\n")
			action, err := Load(root, ".")
			if (err == nil) != test.ok {
				t.Fatalf("Load() error = %v, want success %v", err, test.ok)
			}
			if test.ok && test.name == "ordered strings" {
				want := []string{"first", "", "  ", "--privileged"}
				if !slices.Equal(action.Runs.Args, want) {
					t.Fatalf("runs.args = %#v, want %#v", action.Runs.Args, want)
				}
			}
		})
	}
}

func TestLoadIsStrictAndConfined(t *testing.T) {
	t.Run("softprops top-level env is inert", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", `name: GH Release
description: Github Action for creating Github Releases
author: softprops
inputs:
  token:
    description: Authorized GitHub token or PAT
    required: false
    default: ${{ github.token }}
env:
  GITHUB_TOKEN: "As provided by Github Actions"
runs:
  using: node24
  main: dist/index.js
`)
		action, err := Load(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(action)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "As provided by Github Actions") || action.Runs.Env != nil {
			t.Fatalf("Load() retained top-level env: %s", encoded)
		}
		if token := action.Inputs["token"].Default; token == nil || *token != "${{ github.token }}" {
			t.Fatalf("Load() token input default = %v, want retained input authority", token)
		}
	})

	t.Run("top-level env accepts arbitrary YAML values", func(t *testing.T) {
		for _, value := range []string{
			"placeholder",
			"[GITHUB_TOKEN, {nested: true}]",
			"{GITHUB_TOKEN: '${{ secrets.GITHUB_TOKEN }}', nested: {token: value}}",
		} {
			root := t.TempDir()
			writeAction(t, root, "action.yml", "env: "+value+"\nruns:\n  using: node24\n  main: dist/index.js\n")
			if _, err := Load(root, "."); err != nil {
				t.Fatalf("Load() env %q error = %v", value, err)
			}
		}
	})

	t.Run("official declarative fields", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "name: Setup tool\ndescription: Installs a tool\ndeprecationMessage: Use setup-tool v2 instead\nauthor: GitHub\ninputs:\n  version:\n    deprecationMessage: Use version-file instead\n    type: string\nbranding:\n  icon: package\n  color: blue\nruns:\n  using: node24\n  main: dist/index.js\n")
		action, err := Load(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		if action.Author != "GitHub" {
			t.Fatalf("Load() author = %q, want GitHub", action.Author)
		}
		if action.DeprecationMessage != "Use setup-tool v2 instead" {
			t.Fatalf("Load() deprecation message = %q", action.DeprecationMessage)
		}
		if action.Inputs["version"].DeprecationMessage != "Use version-file instead" {
			t.Fatalf("Load() deprecation message = %q", action.Inputs["version"].DeprecationMessage)
		}
		if action.Inputs["version"].Type != "string" {
			t.Fatalf("Load() input type = %q", action.Inputs["version"].Type)
		}
		if action.Branding.Icon != "package" || action.Branding.Color != "blue" {
			t.Fatalf("Load() branding = %#v", action.Branding)
		}
	})

	t.Run("kebab-case input deprecation message", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "inputs:\n  version:\n    deprecation-message: Use version-file instead\nruns:\n  using: node24\n  main: dist/index.js\n")
		action, err := Load(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		if action.Inputs["version"].DeprecationMessage != "Use version-file instead" {
			t.Fatalf("Load() deprecation message = %q", action.Inputs["version"].DeprecationMessage)
		}
	})

	t.Run("duplicate input deprecation message spellings", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "inputs:\n  version:\n    deprecation-message: Use version-file instead\n    deprecationMessage: Duplicate\nruns:\n  using: node24\n  main: dist/index.js\n")
		if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), "declares both") {
			t.Fatalf("Load() error = %v, want duplicate spelling rejection", err)
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "env:\n  GITHUB_TOKEN: ignored\nunexpected: true\nruns:\n  using: node24\n")
		if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), "line 3: field unexpected not found") {
			t.Fatalf("Load() error = %v, want unknown field rejection", err)
		}
	})

	t.Run("unknown nested field", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "env:\n  GITHUB_TOKEN: ignored\nbranding:\n  unexpected: true\nruns:\n  using: node24\n")
		if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), "line 4: field unexpected not found") {
			t.Fatalf("Load() error = %v, want nested unknown field rejection", err)
		}
	})

	t.Run("malformed top-level env", func(t *testing.T) {
		root := t.TempDir()
		writeAction(t, root, "action.yml", "env: [unterminated\nruns:\n  using: node24\n")
		if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), "parse action metadata") {
			t.Fatalf("Load() error = %v, want malformed YAML rejection", err)
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

func TestLoadRejectsCompositeControlsWithLocation(t *testing.T) {
	for _, control := range []string{"background", "wait", "wait-all", "cancel", "parallel"} {
		t.Run(control, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeAction(t, root, "action.yml", "runs:\n  using: composite\n  steps:\n    - id: child\n      run: echo ok\n      "+control+": true\n")
			_, err = Load(root, ".")
			want := fmt.Sprintf("parse action metadata %q:6:7: composite child 1 (id %q) declares unsupported control %q", filepath.Join(root, "action.yml"), "child", control)
			if err == nil || err.Error() != want {
				t.Fatalf("Load() error = %v, want %q", err, want)
			}
		})
	}
}

func TestLoadAcceptsCompositeContinueOnError(t *testing.T) {
	root := t.TempDir()
	writeAction(t, root, "action.yml", "runs:\n  using: composite\n  steps:\n    - run: exit 1\n      shell: sh\n      continue-on-error: true\n")
	action, err := Load(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if !action.Runs.Steps[0].ContinueOnError {
		t.Fatal("Load() did not retain composite continue-on-error")
	}
}

func TestLoadRejectsInvalidCompositeStepSyntax(t *testing.T) {
	tests := []struct {
		name    string
		steps   string
		wantErr string
	}{
		{name: "both run and uses", steps: "    - run: echo no\n      uses: ./other\n", wantErr: "composite child 1 declares both run and uses"},
		{name: "no execution", steps: "    - id: empty\n", wantErr: "composite child 1 has no run or uses execution"},
		{name: "with on run", steps: "    - run: echo no\n      with:\n        value: no\n", wantErr: "composite run child 1 may not declare with"},
		{name: "duplicate ids", steps: "    - id: Same\n      run: echo one\n    - id: same\n      run: echo two\n", wantErr: `duplicate case-insensitive composite child id "same"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeAction(t, root, "action.yml", "runs:\n  using: composite\n  steps:\n"+test.steps)
			if _, err := Load(root, "."); err == nil || !strings.Contains(err.Error(), test.wantErr) || !strings.Contains(err.Error(), filepath.Join(root, "action.yml")) {
				t.Fatalf("Load() error = %v, want source-located %q", err, test.wantErr)
			}
		})
	}
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

func TestValidateEntrypointsRejectsNestedGitMetadata(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.js", ".gitignore"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("entry"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "index.js"), []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "index.js"), []byte("unverified"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		phase     string
		configure func(*Runs)
	}{
		{name: "pre", phase: "pre", configure: func(r *Runs) { r.Pre = ".git/index.js" }},
		{name: "main", phase: "main", configure: func(r *Runs) { r.Main = "other/../.git/index.js" }},
		{name: "case-insensitive main", phase: "main", configure: func(r *Runs) { r.Main = ".GIT/index.js" }},
		{name: "post", phase: "post", configure: func(r *Runs) { r.Post = ".git" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runs := Runs{Main: "index.js"}
			test.configure(&runs)
			err := (Metadata{Path: root, Runs: runs}).ValidateEntrypoints(RuntimeNode24)
			if err == nil || !strings.Contains(err.Error(), test.phase+" entry point") || !strings.Contains(err.Error(), "excluded from verified action source") {
				t.Fatalf("ValidateEntrypoints() error = %v, want %s exclusion", err, test.phase)
			}
		})
	}

	for _, entry := range []string{".github/index.js", ".gitignore", "index.js"} {
		t.Run("allows "+entry, func(t *testing.T) {
			metadata := Metadata{Path: root, Runs: Runs{Main: entry}}
			if err := metadata.ValidateEntrypoints(RuntimeNode24); err != nil {
				t.Fatalf("ValidateEntrypoints() = %v, want nil", err)
			}
		})
	}

	t.Run("action root beneath repository git directory", func(t *testing.T) {
		metadata := Metadata{Path: filepath.Join(root, ".git"), Runs: Runs{Main: "index.js"}}
		if err := metadata.ValidateEntrypoints(RuntimeNode24); err != nil {
			t.Fatalf("ValidateEntrypoints() = %v, want nil", err)
		}
	})
}

func TestValidateEntrypointsAllowsRemoteActionRepositoryBuildOutput(t *testing.T) {
	repository := canonicalTempDir(t)
	action := filepath.Join(repository, "setup-gradle")
	entrypoint := filepath.Join(repository, "dist", "setup-gradle", "main", "index.js")
	if err := os.MkdirAll(action, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := Metadata{
		Path: action, SourceRoot: repository,
		Runs: Runs{Using: "node24", Main: "../dist/setup-gradle/main/index.js"},
	}
	if err := metadata.ValidateEntrypoints(RuntimeNode24); err != nil {
		t.Fatalf("ValidateEntrypoints() = %v, want repository-confined entry point", err)
	}
	metadata.SourceRoot = action
	if err := metadata.ValidateEntrypoints(RuntimeNode24); err == nil || !strings.Contains(err.Error(), "escapes action source") {
		t.Fatalf("workspace-confined ValidateEntrypoints() error = %v", err)
	}
}

func writeAction(t *testing.T, root, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
