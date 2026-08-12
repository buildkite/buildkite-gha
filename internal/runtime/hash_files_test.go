package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/source"
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

func TestHashWorkspaceFilesBoundsSingleDirectoryEnumeration(t *testing.T) {
	workspace := t.TempDir()
	for i := range 300 {
		writeFixtureFile(t, workspace, fmt.Sprintf("entry-%03d", i), "")
	}
	limits := defaultHashFilesLimits
	limits.entries = 10
	if _, err := hashWorkspaceFilesWithLimits(context.Background(), workspace, []string{"missing"}, limits, false); err == nil || !strings.Contains(err.Error(), "more than 10 entries") {
		t.Fatalf("single-directory entry bound error = %v", err)
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

func TestHashWorkspaceFilesDetectsDirectoryReplacementBeforeTraversal(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "original")
	writeFixtureFile(t, workspace, "target/value", "replacement")
	limits := defaultHashFilesLimits
	replaced := false
	limits.beforeDirectoryOpen = func(name string) {
		if name != "nested" || replaced {
			return
		}
		replaced = true
		if err := os.Rename(filepath.Join(workspace, "nested"), filepath.Join(workspace, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target", filepath.Join(workspace, "nested")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if _, err := hashWorkspaceFilesWithLimits(context.Background(), workspace, []string{"nested/**"}, limits, false); err == nil || !strings.Contains(err.Error(), "changed before traversal") {
		t.Fatalf("directory replacement error = %v", err)
	}
}

func TestHashWorkspaceFilesOpensMatchesThroughPinnedDirectories(t *testing.T) {
	workspace := t.TempDir()
	writeFixtureFile(t, workspace, "nested/value", "original")
	writeFixtureFile(t, workspace, "replacement/value", "replacement")
	limits := defaultHashFilesLimits
	limits.beforeOpen = func(name string) {
		if name != "nested/value" {
			return
		}
		if err := os.Rename(filepath.Join(workspace, "nested"), filepath.Join(workspace, "original")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(workspace, "replacement"), filepath.Join(workspace, "nested")); err != nil {
			t.Fatal(err)
		}
	}
	limits.afterFileHash = func(name string) {
		if name != "nested/value" {
			return
		}
		if err := os.Rename(filepath.Join(workspace, "nested"), filepath.Join(workspace, "replacement")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(workspace, "original"), filepath.Join(workspace, "nested")); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := hashWorkspaceFilesWithLimits(context.Background(), workspace, []string{"nested/value"}, limits, false)
	if err != nil {
		t.Fatal(err)
	}
	want := githubHash("original")
	if digest != want {
		t.Fatalf("digest = %q, want original file digest %q", digest, want)
	}
}

func TestHashFilesInterpolationUsesStepTimeoutContext(t *testing.T) {
	for _, test := range []struct {
		name      string
		env       map[string]string
		condition string
	}{
		{name: "environment", env: map[string]string{"HASH": "${{ hashFiles('large') }}"}},
		{name: "condition", condition: "hashFiles('large') != ''"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixtureFile(t, workspace, ".github/workflows/test.yml", "name: hashFiles timeout\n")
			large := filepath.Join(workspace, "large")
			file, err := os.Create(large)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(256 << 20); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			job := runtimePlan(t, workspace, ".github/workflows/test.yml", []plan.Step{{
				ID:             "hash",
				Kind:           "run",
				Shell:          "sh",
				TimeoutMinutes: 0.001,
				Env:            test.env,
				Condition:      test.condition,
				Command:        "true",
			}})
			started := time.Now()
			_, err = (Runner{}).RunJob(context.Background(), job, workspace)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("RunJob() timeout error = %v", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("RunJob() took %s after step timeout", elapsed)
			}
		})
	}
}

func TestHashFilesPrePhaseUsesStepTimeoutContext(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: hashFiles pre timeout\n")
	file, err := os.Create(filepath.Join(workspace, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(256 << 20); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action/action.yml", "name: timeout\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "action/pre.js", "")
	writeFixtureFile(t, remote, "action/main.js", "")
	digest := digestTree(t, remote)
	lockID := remoteLifecycleLockID(1)
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{
		ID:             "hash",
		Kind:           "uses",
		Uses:           remoteLifecycleUses("action"),
		Action:         &plan.ActionSelector{Lock: lockID},
		TimeoutMinutes: 0.001,
		Env:            map[string]string{"HASH": "${{ hashFiles('large') }}"},
	}})
	job.Schema = plan.SchemaV3
	job.RequiredCapabilities = []string{"network"}
	job.Actions = []plan.ActionLock{remoteLifecycleLock(lockID, "action", digest, nil)}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: digest}}
	started := time.Now()
	_, err = (Runner{Actions: materializer}).RunJob(context.Background(), job, workspace)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJob() pre timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("RunJob() pre took %s after step timeout", elapsed)
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
