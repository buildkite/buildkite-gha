package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/expression"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"golang.org/x/sys/unix"
)

func TestHashWorkspaceFilesConformance(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "a.txt", "alpha")
	writeFixtureFile(t, workspace, "b.txt", "bravo")
	writeFixtureFile(t, workspace, "nested/c.txt", "charlie")
	writeFixtureFile(t, workspace, "nested/skip.txt", "skip")
	writeFixtureFile(t, workspace, ".hidden", "hidden")
	writeFixtureFile(t, workspace, ".config/value", "dot-directory")
	writeFixtureFile(t, workspace, "literal/{name}.txt", "braces")

	for _, test := range []struct {
		name     string
		patterns []string
		contents []string
	}{
		{name: "known digest and stable order", patterns: []string{"*.txt"}, contents: []string{"alpha", "bravo"}},
		{name: "multiple patterns", patterns: []string{"a.txt", "nested/*.txt"}, contents: []string{"alpha", "charlie", "skip"}},
		{name: "ordered negation", patterns: []string{"**", "!nested/**"}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces"}},
		{name: "ordered re-inclusion", patterns: []string{"**", "!nested/**", "nested/c.txt"}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces", "charlie"}},
		{name: "later exclusion", patterns: []string{"nested/c.txt", "!nested/**"}},
		{name: "duplicate overlap", patterns: []string{"a.txt", "*.txt", "a.txt"}, contents: []string{"alpha", "bravo"}},
		{name: "empty matches", patterns: []string{"missing/**"}},
		{name: "hidden files and nested paths", patterns: []string{"**/.hidden", ".config"}, contents: []string{"dot-directory", "hidden"}},
		{name: "directory implies descendants", patterns: []string{"nested"}, contents: []string{"charlie", "skip"}},
		{name: "trailing slash directory", patterns: []string{"nested/"}, contents: []string{"charlie", "skip"}},
		{name: "braces are literal", patterns: []string{"literal/{name}.txt"}, contents: []string{"braces"}},
		{name: "comments and blank patterns", patterns: []string{" # ignored", "", "a.txt"}, contents: []string{"alpha"}},
		{name: "workspace root", patterns: []string{"."}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces", "charlie", "skip"}},
		{name: "spaced negation", patterns: []string{"**", "! nested/**"}, contents: []string{"dot-directory", "hidden", "alpha", "bravo", "braces"}},
		{name: "spaced double negation", patterns: []string{"!! nested/c.txt"}, contents: []string{"charlie"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := hashWorkspaceFiles(context.Background(), workspace, test.patterns)
			if err != nil {
				t.Fatal(err)
			}
			want := githubHash(test.contents...)
			if test.name == "known digest and stable order" {
				want = "90d39555bb3c223e12f5a375c3011d2462fe2e1e36b8416a0b623d5831a9b4f3"
			}
			if got != want {
				t.Fatalf("hashWorkspaceFiles(%#v) = %q, want %q", test.patterns, got, want)
			}
		})
	}

	first, err := hashWorkspaceFiles(context.Background(), workspace, []string{"**"})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		got, err := hashWorkspaceFiles(context.Background(), workspace, []string{"**"})
		if err != nil || got != first {
			t.Fatalf("repeated hash = %q, %v; want %q", got, err, first)
		}
	}
}

func TestHashWorkspaceFilesDirectoryPatternDoesNotMatchRegularFile(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "plain", "regular")
	if got, err := hashWorkspaceFiles(context.Background(), workspace, []string{"plain/"}); err != nil || got != "" {
		t.Fatalf("directory-only regular file hash = %q, %v", got, err)
	}
}

func TestHashFilePatternPlatformCaseSensitivity(t *testing.T) {
	patterns, err := parseHashFilePatterns([]string{"SRC/*.GO"}, defaultHashFilesLimits)
	if err != nil {
		t.Fatal(err)
	}
	if matched, _ := hashFilePatternMatch(patterns, "src/main.go", false); matched {
		t.Fatal("case-sensitive platform matched different case")
	}
	if matched, _ := hashFilePatternMatch(patterns, "src/main.go", true); !matched {
		t.Fatal("case-insensitive platform did not match different case")
	}
}

func TestHashWorkspaceFilesRejectsEscapesAndUnsafeFiles(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	writeFixtureFile(t, workspace, "safe.txt", "safe")
	writeFixtureFile(t, outside, "outside.txt", "outside")
	for _, pattern := range []string{"/etc/passwd", "../outside", "safe/../../outside", `C:\outside`, "./safe/../outside"} {
		t.Run(pattern, func(t *testing.T) {
			if _, err := hashWorkspaceFiles(context.Background(), workspace, []string{pattern}); err == nil || !strings.Contains(err.Error(), "hashFiles pattern") {
				t.Fatalf("escape pattern error = %v", err)
			}
		})
	}
	for _, pattern := range []string{"safe\n", "safe\t", "safe\x1b", "safe\x7f"} {
		if _, err := hashWorkspaceFiles(context.Background(), workspace, []string{pattern}); err == nil || !strings.Contains(err.Error(), "control character") {
			t.Fatalf("control pattern error = %v", err)
		}
	}

	for name, target := range map[string]string{
		"inside-link": filepath.Join(workspace, "safe.txt"),
		"escape-link": filepath.Join(outside, "outside.txt"),
		"loop":        "loop",
	} {
		if err := os.Symlink(target, filepath.Join(workspace, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := hashWorkspaceFiles(context.Background(), workspace, []string{name}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("%s error = %v", name, err)
		}
	}

	if runtime.GOOS != "windows" {
		fifo := filepath.Join(workspace, "pipe")
		if err := unix.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := hashWorkspaceFiles(context.Background(), workspace, []string{"pipe"}); err == nil || !strings.Contains(err.Error(), "non-regular") {
			t.Fatalf("special file error = %v", err)
		}
	}
}

func TestHashWorkspaceRootRemainsPinnedAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, workspace, "value", "inside")
	writeFixtureFile(t, outside, "value", "outside")
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, workspace); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := hashWorkspaceRootFilesWithLimits(context.Background(), root, []string{"value"}, defaultHashFilesLimits, false)
	if err != nil || got != githubHash("inside") {
		t.Fatalf("pinned workspace hash = %q, %v", got, err)
	}
}

func TestRunJobPinsHashWorkspaceBeforePathReplacement(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: pinned hash workspace\n")
	writeFixtureFile(t, workspace, "value", "inside")
	writeFixtureFile(t, outside, "value", "outside")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{
		{ID: "replace", Kind: "run", Shell: "sh", Env: map[string]string{"OUTSIDE": outside}, Command: `mv "$GITHUB_WORKSPACE" "$GITHUB_WORKSPACE-moved" && ln -s "$OUTSIDE" "$GITHUB_WORKSPACE"`},
		{ID: "hash", Kind: "run", Shell: "sh", Env: map[string]string{"VALUE_HASH": "${{ hashFiles('value') }}"}, Command: "test \"$VALUE_HASH\" = " + githubHash("inside")},
	})
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() pinned workspace result = %#v, %v", result, err)
	}
}

func TestRunJobHashFilesArgumentsUseStepEnvironment(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles step environment\n")
	writeFixtureFile(t, workspace, "value", "contents")
	digest := githubHash("contents")
	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{
		ID:        "hash",
		Kind:      "run",
		Shell:     "sh",
		Env:       map[string]string{"PATTERN": "value"},
		Condition: "hashFiles(env.PATTERN) != ''",
		Command:   `test "${{ hashFiles(env.PATTERN) }}" = "` + digest + `"`,
	}})
	result, err := (Runner{}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() step environment hashFiles result = %#v, %v", result, err)
	}
}

func TestHashFilesRemainsUnavailableOutsideWorkflowStepFields(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles surfaces\n")
	writeFixtureFile(t, workspace, "value", "contents")
	for _, test := range []struct {
		name   string
		change func(*plan.Job)
	}{
		{name: "job default shell", change: func(job *plan.Job) { job.DefaultShell = "${{ hashFiles('value') }}" }},
		{name: "job default working directory", change: func(job *plan.Job) { job.DefaultWorkingDirectory = "${{ hashFiles('value') }}" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "run", Kind: "run", Command: "true"}})
			test.change(&job)
			if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "unsupported expression reference") {
				t.Fatalf("RunJob() default hashFiles error = %v", err)
			}
		})
	}

	job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "name", Name: "${{ hashFiles('value') }}", Kind: "run", Shell: "sh", Command: "true"}})
	if result, err := (Runner{}).RunJob(context.Background(), job, workspace); err != nil || result.Conclusion != "success" {
		t.Fatalf("step name unexpectedly evaluated hashFiles: %#v, %v", result, err)
	}

	writeFixtureFile(t, workspace, ".github/actions/composite/action.yml", "runs:\n  using: composite\n  steps:\n    - shell: sh\n      run: echo \"${{ hashFiles('value') }}\"\n")
	job = runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{ID: "composite", Kind: "uses", Uses: "./.github/actions/composite"}})
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "unsupported expression reference") {
		t.Fatalf("composite metadata hashFiles error = %v", err)
	}
}

func TestHashWorkspaceFilesEnforcesEveryBound(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "one", "1")
	writeFixtureFile(t, workspace, "two", "22")
	base := hashFilesLimits{patterns: 10, patternBytes: 20, totalPatternBytes: 40, matches: 10, bytes: 10, entries: 10}
	for _, test := range []struct {
		name     string
		patterns []string
		limits   hashFilesLimits
		want     string
	}{
		{name: "pattern count", patterns: []string{"one", "two"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.patterns = 1 }), want: "1 to 1 patterns"},
		{name: "pattern length", patterns: []string{"one"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.patternBytes = 2 }), want: "exceeds 2 bytes"},
		{name: "total pattern length", patterns: []string{"one", "two"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.totalPatternBytes = 5 }), want: "exceed 5 total bytes"},
		{name: "matched files", patterns: []string{"*"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.matches = 1 }), want: "more than 1 files"},
		{name: "hashed bytes", patterns: []string{"*"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.bytes = 2 }), want: "selected bytes exceed 2"},
		{name: "workspace entries", patterns: []string{"missing"}, limits: withHashLimits(base, func(l *hashFilesLimits) { l.entries = 1 }), want: "more than 1 entries"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := hashWorkspaceFilesWithLimits(context.Background(), workspace, test.patterns, test.limits, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("bound error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestHashWorkspaceFilesDetectsMutationAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "mutable", "before")
	limits := defaultHashFilesLimits
	limits.beforeOpen = func(string) {
		if err := os.WriteFile(filepath.Join(workspace, "mutable"), []byte("after mutation"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hashWorkspaceFilesWithLimits(context.Background(), workspace, []string{"mutable"}, limits, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mutation error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashWorkspaceFiles(ctx, workspace, []string{"mutable"}); err == nil {
		t.Fatal("cancelled hashFiles succeeded")
	}
}

func TestHashFilesInterpolationUsesStepTimeoutContext(t *testing.T) {
	called := false
	eval := expression.Context{HashFilesContext: func(ctx context.Context, _ []string) (string, error) {
		called = true
		<-ctx.Done()
		return "", ctx.Err()
	}}
	execution := (Runner{}).executePlanStep(
		context.Background(), context.Background(), nil, "", plan.Job{},
		plan.Step{ID: "hash", Kind: "run", TimeoutMinutes: 0.001, Env: map[string]string{"HASH": "${{ hashFiles('value') }}"}},
		"0", nil, nil, eval, nil, nil, nil,
	)
	if !called || !errors.Is(execution.err, context.DeadlineExceeded) {
		t.Fatalf("step timeout hashFiles execution = %#v", execution)
	}
}

func githubHash(contents ...string) string {
	if len(contents) == 0 {
		return ""
	}
	combined := sha256.New()
	for _, content := range contents {
		digest := sha256.Sum256([]byte(content))
		_, _ = combined.Write(digest[:])
	}
	return hex.EncodeToString(combined.Sum(nil))
}

func withHashLimits(base hashFilesLimits, update func(*hashFilesLimits)) hashFilesLimits {
	copy := base
	update(&copy)
	return copy
}

func TestGitHubHashTestHelperUsesBinaryDigests(t *testing.T) {
	first, second := sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b"))
	joined := append(append([]byte(nil), first[:]...), second[:]...)
	want := sha256.Sum256(joined)
	if got := githubHash("a", "b"); got != hex.EncodeToString(want[:]) {
		t.Fatalf("githubHash() = %q", got)
	}
	if reflect.DeepEqual(first[:], []byte(hex.EncodeToString(first[:]))) {
		t.Fatal("test helper unexpectedly hashes hexadecimal digests")
	}
}
