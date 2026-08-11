package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

func TestAnonymousCheckoutAdapterPopulatesVerifiedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	workflowSource := []byte("name: checked out\n")
	localSource := []byte("name: local\nruns:\n  using: composite\n  steps:\n    - shell: sh\n      run: echo 'CHECKOUT_CHAIN=ok' >> \"$GITHUB_ENV\"\n")
	localFixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(localFixture, "action.yml"), localSource, 0o644); err != nil {
		t.Fatal(err)
	}
	localDigest, err := source.DigestTree(localFixture)
	if err != nil {
		t.Fatal(err)
	}

	remote := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "action.yml"), []byte("name: checkout\nruns:\n  using: node24\n  main: dist/index.js\n  post: dist/index.js\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(remote, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "dist", "index.js"), []byte("throw new Error('adapter must not execute checkout JavaScript')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remoteDigest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}

	sha := strings.Repeat("a", 40)
	gitLog := filepath.Join(t.TempDir(), "git.log")
	git := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
test "$GIT_CONFIG_NOSYSTEM" = 1
test "$GIT_TERMINAL_PROMPT" = 0
test -z "$GIT_ASKPASS"
test -z "${GH_TOKEN:-}"
case "$HOME" in */.no-home) test ! -e "$HOME" ;; *) exit 21 ;; esac
case "$*" in *credential.helper=*http.extraheader=*core.hooksPath=/dev/null*) ;; *) exit 20 ;; esac
printf '%s\n' "$*" >> ` + shellTestQuote(gitLog) + `
operation=
for argument in "$@"; do
  case "$argument" in init|remote|fetch|checkout) operation="$argument"; break ;; esac
done
case "$operation" in
  init) mkdir -p .git ;;
  checkout)
    printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD
    mkdir -p .github/workflows .github/actions/local
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(workflowSource)) + ` | base64 -d > .github/workflows/test.yml
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(localSource)) + ` | base64 -d > .github/actions/local/action.yml
    chmod 0644 .github/workflows/test.yml .github/actions/local/action.yml
    ;;
esac
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	workflowDigest := sha256.Sum256(workflowSource)
	checkoutID, localID := "a-0000000000000001", "a-0000000000000002"
	job := plan.Job{
		Schema: plan.SchemaV3,
		Compiler: plan.Compiler{
			Version: "phase4-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "checkout",
		},
		Event: plan.Event{
			Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64), Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha,
		},
		Target:               plan.Target{StepKey: "gha-checkout", Queue: "trusted"},
		RequiredCapabilities: []string{"network", "provider-token-read"},
		Steps: []plan.Step{
			{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: checkoutID}},
			{ID: "local", Kind: "uses", Uses: "./.github/actions/local", Action: &plan.ActionSelector{Lock: localID}},
		},
		Actions: []plan.ActionLock{
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: remoteDigest},
			{ID: localID, Source: "workspace", Path: ".github/actions/local", SourceDigest: localDigest},
		},
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}
	result, err := (Runner{Git: git, Actions: materializer}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" || result.Env["CHECKOUT_CHAIN"] != "ok" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if got := result.Outputs; len(got) != 0 {
		t.Fatalf("job outputs = %#v, want no declared job outputs", got)
	}
	logBytes, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	for _, required := range []string{
		"init --template= .",
		"remote add origin https://github.com/buildkite/buildkite-gha.git",
		"fetch --no-tags --no-recurse-submodules --progress --depth=1 origin " + sha,
		"checkout --detach " + sha,
	} {
		if !strings.Contains(log, required) {
			t.Fatalf("Git log lacks %q:\n%s", required, log)
		}
	}
	if strings.Contains(strings.ToLower(log), "authorization") || strings.Contains(strings.ToLower(log), "token") || strings.Contains(log, "git-credentials-helper") {
		t.Fatalf("Git log contains credential material: %s", log)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, ".git", "HEAD")); err != nil || strings.TrimSpace(string(got)) != sha {
		t.Fatalf("checkout HEAD = %q, %v", got, err)
	}

	job.Actions[0].Commit = strings.Repeat("0", 40)
	unknownWorkspace := t.TempDir()
	previousLog := filepath.Join(filepath.Dir(gitLog), "successful.log")
	if err := os.Rename(gitLog, previousLog); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Git: git, Actions: materializer}).RunJob(context.Background(), job, unknownWorkspace); err == nil || !strings.Contains(err.Error(), "does not admit") {
		t.Fatalf("unknown checkout commit error = %v", err)
	}
	if _, err := os.Stat(gitLog); !os.IsNotExist(err) {
		t.Fatalf("Git ran before unknown checkout commit rejection: %v", err)
	}

	job.Actions[0].Commit = actionintegration.CheckoutV7Commit
	job.Actions[0].SourceDigest = "sha256:" + strings.Repeat("0", 64)
	secondWorkspace := t.TempDir()
	if _, err := (Runner{Git: git, Actions: materializer}).RunJob(context.Background(), job, secondWorkspace); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered checkout lock error = %v", err)
	}
	if _, err := os.Stat(gitLog); !os.IsNotExist(err) {
		t.Fatalf("Git ran before tampered lock rejection: %v", err)
	}
}

func TestCheckoutAdapterRejectsUnsupportedInputsAndState(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	processor := newCommandProcessor(io.Discard, io.Discard)
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: repository, SHA: sha}}
	if _, err := (Runner{}).runCheckout(context.Background(), processor, t.TempDir(), job, map[string]string{"token": ""}); err == nil || !strings.Contains(err.Error(), "Phase 6") {
		t.Fatalf("runCheckout() unsupported input error = %v", err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{}).runCheckout(context.Background(), processor, workspace, job, nil); err == nil || !strings.Contains(err.Error(), "empty workspace") {
		t.Fatalf("nonempty workspace error = %v", err)
	}
	job.Event.Provider = "other"
	if _, err := (Runner{}).runCheckout(context.Background(), processor, t.TempDir(), job, nil); err == nil || !strings.Contains(err.Error(), "valid github.com event") {
		t.Fatalf("invalid event error = %v", err)
	}
}

func TestCheckoutFetchDepth(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{name: "default shallow with progress", want: "fetch --no-tags --no-recurse-submodules --progress --depth=1 origin " + sha},
		{name: "explicit shallow", inputs: map[string]string{"fetch-depth": "1"}, want: "fetch --no-tags --no-recurse-submodules --progress --depth=1 origin " + sha},
		{name: "shallow tags", inputs: map[string]string{"fetch-tags": "TRUE"}, want: "fetch --no-tags --no-recurse-submodules --progress --depth=1 origin " + sha + " +refs/tags/*:refs/tags/*"},
		{name: "progress disabled", inputs: map[string]string{"show-progress": "false"}, want: "fetch --no-tags --no-recurse-submodules --depth=1 origin " + sha},
		{
			name: "all branches and tags", inputs: map[string]string{"Fetch-Depth": "0"},
			want: "fetch --no-tags --no-recurse-submodules --progress --prune origin +refs/heads/*:refs/remotes/origin/* +refs/tags/*:refs/tags/* +" + sha + ":refs/buildkite-gha/event",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := strings.Join(checkoutFetchArgs(test.inputs, sha), " "); got != test.want {
				t.Fatalf("checkoutFetchArgs() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCheckoutRefOutput(t *testing.T) {
	eventRef := "refs/heads/main"
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{name: "omitted", want: eventRef},
		{name: "explicit empty", inputs: map[string]string{"REF": ""}, want: eventRef},
		{name: "explicit SHA", inputs: map[string]string{"ref": sha}, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := checkoutRefOutput(test.inputs, eventRef); got != test.want {
				t.Fatalf("checkoutRefOutput() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCheckoutSubmoduleInputAndURLNormalization(t *testing.T) {
	if got := checkoutSubmoduleMode(map[string]string{"SuBmOdUlEs": " ReCuRsIvE "}); got != "recursive" {
		t.Fatalf("checkoutSubmoduleMode() = %q", got)
	}
	for _, test := range []struct{ raw, parent, want string }{
		{"git@github.com:owner/child", "https://github.com/owner/parent.git", "https://github.com/owner/child.git"},
		{"../child.git", "https://github.com/owner/parent.git", "https://github.com/owner/child.git"},
	} {
		got, err := canonicalCheckoutSubmoduleURL(test.raw, test.parent)
		if err != nil || got != test.want {
			t.Fatalf("canonicalCheckoutSubmoduleURL(%q) = %q, %v; want %q", test.raw, got, err, test.want)
		}
	}
	for _, raw := range []string{"file:///tmp/repo", "http://github.com/a/b", "https://code.example.org/team/child.git", "https://GitHub.com/a/b", "https://github.com:443/a/b", "https://user@github.com/a/b", "https://github.com/a/b/c", "https://github.com/a/b?x=1", "https://github.com/a/b#x", "https://github.com/a/%62", "ext::sh -c evil"} {
		if got, err := canonicalCheckoutSubmoduleURL(raw, "https://github.com/owner/parent.git"); err == nil {
			t.Fatalf("canonicalCheckoutSubmoduleURL(%q) = %q, want rejection", raw, got)
		}
	}
}

func TestCheckoutSubmoduleMaterializationUsesRealObjectDatabases(t *testing.T) {
	grand, grandOID := createSubmoduleFixtureRepository(t, "grand.txt", "grand\n", nil)
	childModules := []testGitlink{{name: "nested.module", path: "deps/grand", url: "../grand.git", oid: grandOID}}
	child, childOID := createSubmoduleFixtureRepository(t, "child.txt", "child\n", childModules)
	for _, test := range []struct {
		name      string
		recursive bool
	}{
		{name: "direct"},
		{name: "recursive", recursive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			parentModules := []testGitlink{{name: "child.with.dots", path: "deps/child", url: "https://github.com/owner/child.git", oid: childOID}}
			parent, parentOID := createSubmoduleFixtureRepository(t, "parent.txt", "parent\n", parentModules)
			base := checkoutGitBaseArgs()
			// The production parser has already admitted only github.com URLs. This
			// test-only routing replaces HTTPS with local bare fixture repositories.
			base = append(base, "-c", "protocol.file.allow=always")
			state := checkoutSubmoduleState{
				runner: Runner{Stdout: io.Discard, Stderr: io.Discard}, processor: newCommandProcessor(io.Discard, io.Discard),
				ctx: context.Background(), git: "git", env: map[string]string{}, base: base,
				depthOne: true, recursive: test.recursive,
				fetchURL: func(raw string) string {
					switch raw {
					case "https://github.com/owner/child.git":
						return child
					case "https://github.com/owner/grand.git":
						return grand
					default:
						t.Fatalf("unexpected validated fetch URL %q", raw)
						return ""
					}
				},
			}
			if err := state.materialize(parent, parentOID, "https://github.com/owner/parent.git", 0); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			childPath := filepath.Join(parent, "deps", "child")
			if got := strings.TrimSpace(runTestGit(t, childPath, "rev-parse", "HEAD")); got != childOID {
				t.Fatalf("child HEAD = %s, want %s", got, childOID)
			}
			grandPath := filepath.Join(childPath, "deps", "grand")
			if test.recursive {
				if got := strings.TrimSpace(runTestGit(t, grandPath, "rev-parse", "HEAD")); got != grandOID {
					t.Fatalf("grandchild HEAD = %s, want %s", got, grandOID)
				}
			} else if _, err := os.Stat(filepath.Join(grandPath, ".git")); !os.IsNotExist(err) {
				t.Fatalf("direct materialization installed grandchild repository: %v", err)
			}
		})
	}
}

type testGitlink struct{ name, path, url, oid string }

func createSubmoduleFixtureRepository(t *testing.T, file, contents string, links []testGitlink) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runTestGit(t, repo, "init", "--initial-branch=main")
	runTestGit(t, repo, "config", "user.name", "buildkite-gha test")
	runTestGit(t, repo, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, file), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", file)
	if len(links) != 0 {
		var manifest strings.Builder
		for _, link := range links {
			fmt.Fprintf(&manifest, "[submodule %q]\n\tpath = %s\n\turl = %s\n", link.name, link.path, link.url)
			runTestGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+link.oid+","+link.path)
		}
		if err := os.WriteFile(filepath.Join(repo, ".gitmodules"), []byte(manifest.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, repo, "add", ".gitmodules")
	}
	runTestGit(t, repo, "commit", "-m", "fixture")
	oid := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runTestGit(t, "", "clone", "--bare", repo, bare)
	return bare, oid
}

type testIndexEntry struct{ mode, path, oid string }

func createManifestFixtureRepository(t *testing.T, manifest []byte, manifestMode string, entries []testIndexEntry) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runTestGit(t, repo, "init", "--initial-branch=main")
	runTestGit(t, repo, "config", "user.name", "buildkite-gha test")
	runTestGit(t, repo, "config", "user.email", "test@example.invalid")
	manifestOID := strings.TrimSpace(runTestGitInput(t, repo, manifest, "hash-object", "-w", "--stdin"))
	if manifestMode != "120000" {
		runTestGit(t, repo, "update-index", "--add", "--cacheinfo", manifestMode+","+manifestOID+",.gitmodules")
	}
	for _, entry := range entries {
		if entry.oid == "" {
			entry.oid = strings.TrimSpace(runTestGitInput(t, repo, nil, "hash-object", "-w", "--stdin"))
		}
		runTestGit(t, repo, "update-index", "--add", "--cacheinfo", entry.mode+","+entry.oid+","+entry.path)
	}
	var tree string
	if manifestMode == "120000" {
		tree = strings.TrimSpace(runTestGitInput(t, repo, []byte("120000 blob "+manifestOID+"\t.gitmodules\n"), "mktree"))
	} else {
		tree = strings.TrimSpace(runTestGit(t, repo, "write-tree"))
	}
	oid := strings.TrimSpace(runTestGitInput(t, repo, []byte("manifest fixture\n"), "commit-tree", tree))
	runTestGit(t, repo, "update-ref", "refs/heads/main", oid)
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runTestGit(t, "", "clone", "--bare", repo, bare)
	return bare, oid
}

func TestCheckoutSubmoduleManifestRejectsUntrustedTreesBeforeFetch(t *testing.T) {
	_, childOID := createSubmoduleFixtureRepository(t, "child.txt", "child\n", nil)
	valid := func(name, path string) string {
		return fmt.Sprintf("[submodule %q]\n\tpath = %s\n\turl = https://github.com/owner/child.git\n", name, path)
	}
	tests := []struct {
		name, manifest, mode string
		entries              []testIndexEntry
	}{
		{name: "unknown update command", manifest: valid("child", "child") + "\tupdate = !command\n", entries: []testIndexEntry{{"160000", "child", childOID}}},
		{name: "include path", manifest: "[include]\n\tpath = /tmp/evil\n"},
		{name: "duplicate path key", manifest: valid("child", "child") + "\tpath = other\n", entries: []testIndexEntry{{"160000", "child", childOID}}},
		{name: "duplicate URL key", manifest: valid("child", "child") + "\turl = https://github.com/owner/other.git\n", entries: []testIndexEntry{{"160000", "child", childOID}}},
		{name: "missing URL", manifest: "[submodule \"child\"]\n\tpath = child\n", entries: []testIndexEntry{{"160000", "child", childOID}}},
		{name: "path is blob", manifest: valid("child", "child"), entries: []testIndexEntry{{mode: "100644", path: "child"}}},
		{name: "literal path mismatch", manifest: valid("child", ":(glob)child"), entries: []testIndexEntry{{"160000", "child", childOID}}},
		{name: "case-fold path", manifest: valid("one", "A/B") + valid("two", "a/b"), entries: []testIndexEntry{{"160000", "A/B", childOID}, {"160000", "a/b", childOID}}},
		{name: "nonadjacent case-fold ancestor", manifest: valid("one", "A") + valid("guard", "a-guard") + valid("two", "a/child"), entries: []testIndexEntry{{"160000", "A", childOID}, {"160000", "a-guard", childOID}, {"160000", "a/child", childOID}}},
		{name: "parent child collision", manifest: valid("one", "deps") + valid("two", "deps/child"), entries: []testIndexEntry{{"160000", "deps", childOID}}},
		{name: "traversal", manifest: valid("child", "../child")},
		{name: "absolute", manifest: valid("child", "/child")},
		{name: "backslash", manifest: valid("child", `deps\child`)},
		{name: "dot git", manifest: valid("child", "deps/.git/child")},
		{name: "control", manifest: valid("child", "deps/\x01child")},
		{name: "symlink manifest", manifest: "target", mode: "120000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode := test.mode
			if mode == "" {
				mode = "100644"
			}
			repo, commit := createManifestFixtureRepository(t, []byte(test.manifest), mode, test.entries)
			fetches := 0
			state := checkoutSubmoduleState{ctx: context.Background(), git: "git", env: map[string]string{}, base: checkoutGitBaseArgs(), fetchURL: func(raw string) string { fetches++; return raw }}
			if err := state.materialize(repo, commit, "https://github.com/owner/parent.git", 0); err == nil {
				t.Fatal("materialize accepted malicious manifest")
			}
			if fetches != 0 {
				t.Fatalf("fetch routing called %d times before manifest rejection", fetches)
			}
		})
	}
}

func TestCheckoutSubmoduleManifestBounds(t *testing.T) {
	_, childOID := createSubmoduleFixtureRepository(t, "child.txt", "child\n", nil)
	var manifest strings.Builder
	entries := make([]testIndexEntry, 0, 129)
	for i := range 129 {
		name, path := fmt.Sprintf("module-%03d", i), fmt.Sprintf("deps/module-%03d", i)
		fmt.Fprintf(&manifest, "[submodule %q]\n\tpath = %s\n\turl = https://github.com/owner/child.git\n", name, path)
		entries = append(entries, testIndexEntry{"160000", path, childOID})
	}
	repo, commit := createManifestFixtureRepository(t, []byte(manifest.String()), "100644", entries)
	state := checkoutSubmoduleState{ctx: context.Background(), git: "git", env: map[string]string{}, base: checkoutGitBaseArgs()}
	if _, err := state.manifest(repo, commit, "https://github.com/owner/parent.git"); err == nil || !strings.Contains(err.Error(), "entry bound") {
		t.Fatalf("129-entry manifest error = %v", err)
	}

	repo, commit = createManifestFixtureRepository(t, bytes.Repeat([]byte{'x'}, (1<<20)+1), "100644", nil)
	state = checkoutSubmoduleState{ctx: context.Background(), git: "git", env: map[string]string{}, base: checkoutGitBaseArgs()}
	if _, err := state.manifest(repo, commit, "https://github.com/owner/parent.git"); err == nil || !strings.Contains(err.Error(), "size bounds") {
		t.Fatalf("oversized manifest error = %v", err)
	}
}

func TestCheckoutSubmoduleFetchArgumentsIgnoreParentFetchTags(t *testing.T) {
	for _, test := range []struct {
		name     string
		depthOne bool
		want     []string
	}{
		{"depth one", true, []string{"fetch", "--no-tags", "--no-recurse-submodules", "--depth=1", "origin", strings.Repeat("a", 40)}},
		{"depth zero", false, []string{"fetch", "--no-tags", "--no-recurse-submodules", "origin", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*", "+" + strings.Repeat("a", 40) + ":refs/buildkite-gha/submodule"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			oid := strings.Repeat("a", 40)
			if got := checkoutSubmoduleFetchArgs(test.depthOne, oid); !slices.Equal(got, test.want) {
				t.Fatalf("submodule fetch args = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCheckoutSubmodulePathValidation(t *testing.T) {
	for _, valid := range []string{"vendor/child", "a"} {
		if err := validateCheckoutSubmodulePath(valid); err != nil {
			t.Fatalf("validateCheckoutSubmodulePath(%q): %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "../child", "/child", "a\\b", "-child", "a/.git/b", "a//b", "a/child."} {
		if err := validateCheckoutSubmodulePath(invalid); err == nil {
			t.Fatalf("validateCheckoutSubmodulePath(%q) succeeded", invalid)
		}
	}
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "vendor")); err != nil {
		t.Fatal(err)
	}
	if err := preflightCheckoutSubmoduleDestination(root, "vendor/child"); err == nil {
		t.Fatal("symlink ancestor was accepted")
	}
	root = t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "child")); err != nil {
		t.Fatal(err)
	}
	if err := preflightCheckoutSubmoduleDestination(root, "child"); err == nil {
		t.Fatal("final symlink was accepted")
	}
}

func TestPublishCheckoutSubmodulesRollsBackEarlierSibling(t *testing.T) {
	root, staging := t.TempDir(), t.TempDir()
	for i := range 2 {
		if err := os.Mkdir(filepath.Join(staging, fmt.Sprint(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "second"), []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := publishCheckoutSubmodules(context.Background(), root, staging, []checkoutSubmodule{{path: "deps/first"}, {path: "second"}})
	if err == nil {
		t.Fatal("publication succeeded despite second destination collision")
	}
	if _, err := os.Stat(filepath.Join(root, "deps", "first")); !os.IsNotExist(err) {
		t.Fatalf("first sibling remained published after rollback: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "deps")); err != nil || len(entries) != 0 {
		t.Fatalf("rollback residue is not an empty ancestor: %v, entries=%v", err, entries)
	}
	if info, err := os.Stat(filepath.Join(staging, "0")); err != nil || !info.IsDir() {
		t.Fatalf("first sibling was not returned to staging: %v", err)
	}
}

func TestSubmoduleResolvedCredentialsWithoutCapabilityDoNotInvokeHelper(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "helper-ran")
	agent := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\n: > "+shellTestQuote(marker)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := checkoutSubmoduleState{
		runner:    Runner{RepositoryCredentials: &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "secret"}, Stdout: io.Discard, Stderr: io.Discard},
		processor: newCommandProcessor(io.Discard, io.Discard), ctx: context.Background(), git: git,
		base: checkoutGitBaseArgs(), env: map[string]string{}, allowProviderCredentials: false,
	}
	if err := state.stream(t.TempDir(), state.allowProviderCredentials, "fetch", "origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("credential helper ran without provider-token-read authority: %v", err)
	}
}

func TestCredentialedSubmoduleFetchUsesExactRepositoryAndCommandScopedSecret(t *testing.T) {
	workspace := t.TempDir()
	inputLog := filepath.Join(t.TempDir(), "helper-input")
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	agentScript := `#!/bin/sh
set -eu
test "$1" = git-credentials-helper
test "$2" = get
test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
cat > ` + shellTestQuote(inputLog) + `
printf 'username=token\npassword=repository-secret\n'
`
	if err := os.WriteFile(agent, []byte(agentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(t.TempDir(), "git")
	gitScript := `#!/bin/sh
set -eu
helper=
for argument in "$@"; do
  case "$argument" in credential.helper=!*) helper="${argument#credential.helper=!}" ;; esac
done
test -n "$helper"
mkdir -p .git
: > .git/config
printf 'protocol=https\nhost=github.com\npath=owner/private.git\n\n' | sh -c "$helper get" >/dev/null
echo job-secret
`
	if err := os.WriteFile(git, []byte(gitScript), 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	state := checkoutSubmoduleState{
		runner:    Runner{RepositoryCredentials: &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "job-secret"}, Stdout: &logs, Stderr: &logs},
		processor: newCommandProcessor(&logs, &logs), ctx: context.Background(), git: git, base: checkoutGitBaseArgs(), env: map[string]string{}, allowProviderCredentials: true,
	}
	if err := state.stream(workspace, true, "fetch", "origin", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(inputLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != "protocol=https\nhost=github.com\npath=owner/private.git\n\n" {
		t.Fatalf("credential helper input = %q", input)
	}
	if strings.Contains(logs.String(), "job-secret") || !strings.Contains(logs.String(), "***") {
		t.Fatalf("credentialed fetch output was not masked: %q", logs.String())
	}
	config, err := os.ReadFile(filepath.Join(workspace, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "credential") || strings.Contains(string(config), agent) {
		t.Fatalf("credential helper persisted in staged config: %q", config)
	}
}

func TestCheckoutFetchArgsAgainstRealRepository(t *testing.T) {
	sourceRoot := t.TempDir()
	runTestGit(t, sourceRoot, "init", "--initial-branch=main")
	runTestGit(t, sourceRoot, "config", "user.name", "buildkite-gha test")
	runTestGit(t, sourceRoot, "config", "user.email", "test@example.invalid")
	var commits []string
	for i, contents := range []string{"first\n", "second\n", "third\n"} {
		if err := os.WriteFile(filepath.Join(sourceRoot, "payload.txt"), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, sourceRoot, "add", "payload.txt")
		runTestGit(t, sourceRoot, "commit", "-m", fmt.Sprintf("commit %d", i+1))
		commits = append(commits, strings.TrimSpace(runTestGit(t, sourceRoot, "rev-parse", "HEAD")))
	}
	runTestGit(t, sourceRoot, "tag", "old", commits[0])
	runTestGit(t, sourceRoot, "tag", "tip", commits[2])
	remote := filepath.Join(t.TempDir(), "remote.git")
	runTestGit(t, "", "clone", "--bare", sourceRoot, remote)

	for _, test := range []struct {
		name       string
		inputs     map[string]string
		wantCount  string
		wantTagTip bool
	}{
		{name: "depth one exact detached SHA", wantCount: "1"},
		{name: "depth zero history and tags", inputs: map[string]string{"fetch-depth": "0", "show-progress": "false"}, wantCount: "2", wantTagTip: true},
		{name: "shallow explicit tags", inputs: map[string]string{"fetch-tags": "true", "show-progress": "false"}, wantCount: "1", wantTagTip: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			runTestGit(t, workspace, "init", "--initial-branch=main")
			runTestGit(t, workspace, "remote", "add", "origin", remote)
			runTestGit(t, workspace, checkoutFetchArgs(test.inputs, commits[1])...)
			runTestGit(t, workspace, "checkout", "--detach", commits[1])
			if got := strings.TrimSpace(runTestGit(t, workspace, "rev-parse", "HEAD")); got != commits[1] {
				t.Fatalf("HEAD = %s, want exact event SHA %s", got, commits[1])
			}
			if got := strings.TrimSpace(runTestGit(t, workspace, "rev-list", "--count", "HEAD")); got != test.wantCount {
				t.Fatalf("reachable commit count = %s, want %s", got, test.wantCount)
			}
			_, err := exec.Command("git", "-C", workspace, "show-ref", "--verify", "--quiet", "refs/tags/tip").CombinedOutput()
			if (err == nil) != test.wantTagTip {
				t.Fatalf("tip tag present = %t, want %t (error %v)", err == nil, test.wantTagTip, err)
			}
		})
	}
}

func runTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	return runTestGitInput(t, directory, nil, args...)
}

func runTestGitInput(t *testing.T, directory string, input []byte, args ...string) string {
	t.Helper()
	if directory != "" {
		args = append([]string{"-C", directory}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func TestCheckoutSubmoduleCancellationCleansStagingAndProcessGroup(t *testing.T) {
	child, childOID := createSubmoduleFixtureRepository(t, "child.txt", "child\n", nil)
	parent, parentOID := createSubmoduleFixtureRepository(t, "parent.txt", "parent\n", []testGitlink{{"child", "deps/child", "https://github.com/owner/child.git", childOID}})
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	pidFile, fetchStarted := filepath.Join(t.TempDir(), "child.pid"), filepath.Join(t.TempDir(), "fetch-started")
	wrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
operation=
for argument in "$@"; do
  case "$argument" in fetch) operation=fetch; break ;; esac
done
if [ "$operation" = fetch ]; then
  (trap '' INT TERM; sleep 30) &
  echo $! > ` + shellTestQuote(pidFile) + `
  : > ` + shellTestQuote(fetchStarted) + `
  wait
fi
exec ` + shellTestQuote(realGit) + ` "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := checkoutSubmoduleState{
		runner:    Runner{Stdout: io.Discard, Stderr: io.Discard, InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond},
		processor: newCommandProcessor(io.Discard, io.Discard), ctx: ctx, git: wrapper, env: map[string]string{}, base: append(checkoutGitBaseArgs(), "-c", "protocol.file.allow=always"), depthOne: true,
		fetchURL: func(string) string { return child },
	}
	done := make(chan error, 1)
	go func() { done <- state.materialize(parent, parentOID, "https://github.com/owner/parent.git", 0) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(fetchStarted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fetch did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("materialize cancellation error = %v", err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if err := exec.Command("sh", "-c", "kill -0 "+pid).Run(); err == nil {
		t.Fatalf("fetch child process %s remains alive", pid)
	}
	if matches, _ := filepath.Glob(filepath.Join(parent, ".buildkite-gha-submodules-*")); len(matches) != 0 {
		t.Fatalf("staging leaked: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(parent, "deps", "child")); !os.IsNotExist(err) {
		t.Fatalf("destination published after cancellation: %v", err)
	}
}

func TestCheckoutRejectsInvalidRepositoryBeforeInspectingWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "owner/..", SHA: strings.Repeat("a", 40)}}
	processor := newCommandProcessor(io.Discard, io.Discard)
	if _, err := (Runner{}).runCheckout(context.Background(), processor, workspace, job, nil); err == nil || !strings.Contains(err.Error(), "valid github.com event repository") {
		t.Fatalf("checkout repository validation error = %v", err)
	}
}

func TestCheckoutUsesCommandScopedAgentCredentialHelper(t *testing.T) {
	workspace := t.TempDir()
	sha := strings.Repeat("a", 40)
	repositoryToken := "ghs_repository_token"
	proxyEnvironment := map[string]string{
		"HTTP_PROXY": "http://upper-http.example:8080", "HTTPS_PROXY": "http://upper-https.example:8080",
		"ALL_PROXY": "socks5://upper-all.example:1080", "NO_PROXY": "upper-no-proxy.example",
		"http_proxy": "http://lower-http.example:8080", "https_proxy": "http://lower-https.example:8080",
		"all_proxy": "socks5://lower-all.example:1080", "no_proxy": "lower-no-proxy.example",
	}
	for name, value := range proxyEnvironment {
		t.Setenv(name, value)
	}
	gitLog := filepath.Join(t.TempDir(), "git.log")
	agentLog := filepath.Join(t.TempDir(), "agent.log")
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	agentScript := `#!/bin/sh
set -eu
test "$1" = git-credentials-helper
test "$2" = get
test "$BUILDKITE_AGENT_ENDPOINT" = https://agent.example/v3
test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
test "$BUILDKITE_JOB_ID" = 11111111-1111-4111-8111-111111111111
test "$BUILDKITE_NO_HTTP2" = true
test "$HTTP_PROXY" = http://upper-http.example:8080
test "$HTTPS_PROXY" = http://upper-https.example:8080
test "$ALL_PROXY" = socks5://upper-all.example:1080
test "$NO_PROXY" = upper-no-proxy.example
test "$http_proxy" = http://lower-http.example:8080
test "$https_proxy" = http://lower-https.example:8080
test "$all_proxy" = socks5://lower-all.example:1080
test "$no_proxy" = lower-no-proxy.example
input="$(cat)"
case "$input" in
  *"protocol=https"*"host=github.com"*"path=buildkite/buildkite-gha.git"*) ;;
  *) exit 40 ;;
esac
printf '%s\n' "$*" > ` + shellTestQuote(agentLog) + `
printf 'username=token\npassword=%s\n' ` + shellTestQuote(repositoryToken) + `
`
	if err := os.WriteFile(agent, []byte(agentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
assert_no_proxy_environment() {
  test -z "${HTTP_PROXY+x}"
  test -z "${HTTPS_PROXY+x}"
  test -z "${ALL_PROXY+x}"
  test -z "${NO_PROXY+x}"
  test -z "${http_proxy+x}"
  test -z "${https_proxy+x}"
  test -z "${all_proxy+x}"
  test -z "${no_proxy+x}"
}
operation=
for argument in "$@"; do
  case "$argument" in init|remote|fetch|checkout) operation="$argument"; break ;; esac
done
case "$operation" in
  init)
    test -z "$GIT_ASKPASS"
    test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
    test -z "${BUILDKITE_JOB_ID+x}"
    test -z "${BUILDKITE_NO_HTTP2+x}"
    assert_no_proxy_environment
    mkdir -p .git
    ;;
  remote)
    test -z "$GIT_ASKPASS"
    test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
    test -z "${BUILDKITE_JOB_ID+x}"
    test -z "${BUILDKITE_NO_HTTP2+x}"
    assert_no_proxy_environment
    ;;
  fetch)
    test -z "$GIT_ASKPASS"
    test "$BUILDKITE_AGENT_ENDPOINT" = https://agent.example/v3
    test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
    test "$BUILDKITE_JOB_ID" = 11111111-1111-4111-8111-111111111111
    test "$HTTP_PROXY" = http://upper-http.example:8080
    test "$HTTPS_PROXY" = http://upper-https.example:8080
    test "$ALL_PROXY" = socks5://upper-all.example:1080
    test "$NO_PROXY" = upper-no-proxy.example
    test "$http_proxy" = http://lower-http.example:8080
    test "$https_proxy" = http://lower-https.example:8080
    test "$all_proxy" = socks5://lower-all.example:1080
    test "$no_proxy" = lower-no-proxy.example
    helper=
    use_http_path=
    for argument in "$@"; do
      case "$argument" in
        credential.helper=!*) helper="${argument#credential.helper=!}" ;;
        credential.useHttpPath=true) use_http_path=true ;;
      esac
    done
    test -n "$helper"
    test "$use_http_path" = true
    credentials="$(printf 'protocol=https\nhost=github.com\npath=buildkite/buildkite-gha.git\n\n' | sh -c "$helper get")"
    case "$credentials" in
      *"username=token"*"password=` + repositoryToken + `"*) ;;
      *) exit 41 ;;
    esac
    ;;
  checkout)
    test -z "$GIT_ASKPASS"
    test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
    test -z "${BUILDKITE_JOB_ID+x}"
    test -z "${BUILDKITE_NO_HTTP2+x}"
    assert_no_proxy_environment
    printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD
    ;;
esac
printf '%s\n' "$*" >> ` + shellTestQuote(gitLog) + `
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	credentials := &AgentRepositoryCredentials{
		Agent: agent, Endpoint: "https://agent.example/v3", JobID: testCacheJobID, JobToken: "job-secret", NoHTTP2: "true",
	}
	credentials, err := resolveAgentRepositoryCredentialsBeforeWorkflow(credentials)
	if err != nil {
		t.Fatal(err)
	}
	for name := range proxyEnvironment {
		t.Setenv(name, "http://late-workflow-value.invalid")
	}
	result, err := (Runner{Git: git, RepositoryCredentials: credentials, Stdout: &logs, Stderr: &logs}).runCheckout(context.Background(), processor, workspace, job, nil)
	if err != nil {
		t.Fatalf("runCheckout() error = %v, logs = %q", err, logs.String())
	}
	if result.Outputs["commit"] != sha || result.Outputs["ref"] != job.Event.Ref {
		t.Fatalf("checkout outputs = %#v", result.Outputs)
	}
	if _, err := os.Stat(agentLog); err != nil {
		t.Fatalf("Buildkite Agent credential helper did not run: %v", err)
	}
	gitBytes, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gitBytes), repositoryToken) || strings.Contains(logs.String(), repositoryToken) {
		t.Fatalf("checkout exposed repository token in Git arguments or logs: %q / %q", gitBytes, logs.String())
	}
	if strings.Count(string(gitBytes), "git-credentials-helper") != 1 {
		t.Fatalf("credential helper was not confined to one Git command: %q", gitBytes)
	}
}

func TestAgentGitCredentialHelperCommandQuotesExecutable(t *testing.T) {
	got := agentGitCredentialHelperCommand("/tmp/agent's path")
	want := `!'/tmp/agent'\''s path' git-credentials-helper`
	if got != want {
		t.Fatalf("agentGitCredentialHelperCommand() = %q, want %q", got, want)
	}
}

func TestRepositoryProviderCheckoutFailsClosedWhenHelperDeniesAccess(t *testing.T) {
	workspace := t.TempDir()
	checkoutMarker := filepath.Join(t.TempDir(), "checkout-ran")
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	if err := os.WriteFile(agent, []byte("#!/bin/sh\necho job-secret >&2\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(t.TempDir(), "git")
	gitScript := `#!/bin/sh
set -eu
operation=
helper=
for argument in "$@"; do
  case "$argument" in
    init|remote|fetch|checkout) operation="$argument" ;;
    credential.helper=!*) helper="${argument#credential.helper=!}" ;;
  esac
done
case "$operation" in
  init) mkdir -p .git ;;
  fetch)
    test -n "$helper"
    printf 'protocol=https\nhost=github.com\npath=buildkite/buildkite-gha.git\n\n' | sh -c "$helper get"
    ;;
  checkout) : > ` + shellTestQuote(checkoutMarker) + ` ;;
esac
`
	if err := os.WriteFile(git, []byte(gitScript), 0o700); err != nil {
		t.Fatal(err)
	}
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: strings.Repeat("a", 40)},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	credentials := &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "job-secret"}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	if _, err := (Runner{Git: git, RepositoryCredentials: credentials, Stdout: &logs, Stderr: &logs}).runCheckout(context.Background(), processor, workspace, job, nil); err == nil {
		t.Fatal("runCheckout() succeeded after repository-provider helper denial")
	}
	if strings.Contains(logs.String(), "job-secret") || !strings.Contains(logs.String(), "***") {
		t.Fatalf("helper denial logs did not scrub the job credential: %q", logs.String())
	}
	if _, err := os.Stat(checkoutMarker); !os.IsNotExist(err) {
		t.Fatalf("checkout ran after credential helper denial: %v", err)
	}
}

func TestRepositoryProviderCheckoutPinsGitAndAgentBeforeActionPreHooks(t *testing.T) {
	workspace := t.TempDir()
	workflowSource := []byte("name: repository provider checkout executable confinement\n")
	sha := strings.Repeat("a", 40)
	repositoryToken := "ghs_repository_token"

	trustedLog := filepath.Join(t.TempDir(), "trusted-git.log")
	trustedDir := t.TempDir()
	trustedGit := filepath.Join(trustedDir, "git")
	trustedScript := `#!/bin/sh
set -eu
operation=
for argument in "$@"; do
  case "$argument" in init|remote|fetch|checkout) operation="$argument"; break ;; esac
done
case "$operation" in
  init) mkdir -p .git ;;
  fetch)
    helper=
    for argument in "$@"; do
      case "$argument" in credential.helper=!*) helper="${argument#credential.helper=!}" ;; esac
    done
    test -n "$helper"
    credentials="$(printf 'protocol=https\nhost=github.com\npath=buildkite/buildkite-gha.git\n\n' | sh -c "$helper get")"
    case "$credentials" in *"password=` + repositoryToken + `"*) ;; *) exit 41 ;; esac
    ;;
  checkout)
    printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD
    mkdir -p .github/workflows
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(workflowSource)) + ` | base64 -d > .github/workflows/test.yml
    ;;
esac
printf '%s\n' "$*" >> ` + shellTestQuote(trustedLog) + `
`
	if err := os.WriteFile(trustedGit, []byte(trustedScript), 0o700); err != nil {
		t.Fatal(err)
	}
	trustedAgentMarker := filepath.Join(t.TempDir(), "trusted-agent-ran")
	trustedAgent := filepath.Join(trustedDir, "buildkite-agent")
	trustedAgentScript := `#!/bin/sh
set -eu
test "$1" = git-credentials-helper
test "$2" = get
test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
test "$BUILDKITE_JOB_ID" = 11111111-1111-4111-8111-111111111111
input="$(cat)"
case "$input" in *"path=buildkite/buildkite-gha.git"*) ;; *) exit 42 ;; esac
: > ` + shellTestQuote(trustedAgentMarker) + `
printf 'username=token\npassword=%s\n' ` + shellTestQuote(repositoryToken) + `
`
	if err := os.WriteFile(trustedAgent, []byte(trustedAgentScript), 0o700); err != nil {
		t.Fatal(err)
	}

	lookupDir := t.TempDir()
	lookupGit := filepath.Join(lookupDir, "git")
	if err := os.Symlink(trustedGit, lookupGit); err != nil {
		t.Fatal(err)
	}
	lookupAgent := filepath.Join(lookupDir, "buildkite-agent")
	if err := os.Symlink(trustedAgent, lookupAgent); err != nil {
		t.Fatal(err)
	}
	poisonDir := canonicalTempDir(t)
	poisonGitMarker := filepath.Join(t.TempDir(), "poison-git-ran")
	poisonGit := filepath.Join(poisonDir, "git")
	if err := os.WriteFile(poisonGit, []byte("#!/bin/sh\ntouch "+shellTestQuote(poisonGitMarker)+"\nexit 97\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	poisonAgentMarker := filepath.Join(t.TempDir(), "poison-agent-ran")
	poisonAgent := filepath.Join(poisonDir, "buildkite-agent")
	if err := os.WriteFile(poisonAgent, []byte("#!/bin/sh\n: > "+shellTestQuote(poisonAgentMarker)+"\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lookupDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "name: checkout\nruns:\n  using: node24\n  main: dist/index.js\n")
	writeFixtureFile(t, remote, "dist/index.js", "")
	writeFixtureFile(t, remote, "poison/action.yml", "name: poison PATH\nruns:\n  using: node24\n  pre: pre.js\n  main: main.js\n")
	writeFixtureFile(t, remote, "poison/pre.js", "")
	writeFixtureFile(t, remote, "poison/main.js", "")
	remoteDigest := digestTree(t, remote)

	node := filepath.Join(t.TempDir(), "node24")
	nodeScript := `#!/bin/sh
set -eu
if [ "${1:-}" = --version ]; then
  echo v24.0.0
  exit 0
fi
if [ "${1##*/}" = pre.js ]; then
  rm -f "$LOOKUP_GIT"
  ln -s "$POISON_GIT" "$LOOKUP_GIT"
  rm -f "$LOOKUP_AGENT"
  ln -s "$POISON_AGENT" "$LOOKUP_AGENT"
fi
`
	if err := os.WriteFile(node, []byte(nodeScript), 0o700); err != nil {
		t.Fatal(err)
	}

	workflowDigest := sha256.Sum256(workflowSource)
	poisonID, checkoutID := "a-0000000000000001", "a-0000000000000002"
	job := plan.Job{
		Schema: plan.SchemaV3,
		Compiler: plan.Compiler{
			Version: "phase4-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Workflow: plan.Workflow{
			Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "checkout",
		},
		Event: plan.Event{
			Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64), Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha,
		},
		Target:               plan.Target{StepKey: "gha-checkout", Queue: "trusted"},
		RequiredCapabilities: []string{"network", "provider-token-read"},
		Env: map[string]string{
			"LOOKUP_AGENT": lookupAgent,
			"LOOKUP_GIT":   lookupGit,
			"POISON_AGENT": poisonAgent,
			"POISON_GIT":   poisonGit,
		},
		Steps: []plan.Step{
			{ID: "poison", Kind: "uses", Uses: "owner/repo/poison@v1", Action: &plan.ActionSelector{Lock: poisonID}},
			{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: checkoutID}},
		},
		Actions: []plan.ActionLock{
			{ID: poisonID, Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: strings.Repeat("b", 40), Path: "poison", SourceDigest: remoteDigest},
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: remoteDigest},
		},
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: remoteDigest}}
	var logs bytes.Buffer
	credentials := &AgentRepositoryCredentials{JobID: testCacheJobID, JobToken: "job-secret"}
	result, err := (Runner{Node24: node, RepositoryCredentials: credentials, Actions: materializer, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	if _, err := os.Stat(trustedLog); err != nil {
		t.Fatalf("trusted Git did not run: %v", err)
	}
	if target, err := filepath.EvalSymlinks(lookupGit); err != nil || target != poisonGit {
		t.Fatalf("pre-hook Git replacement = %q, %v; want %q", target, err, poisonGit)
	}
	if target, err := filepath.EvalSymlinks(lookupAgent); err != nil || target != poisonAgent {
		t.Fatalf("pre-hook Agent replacement = %q, %v; want %q", target, err, poisonAgent)
	}
	if _, err := os.Stat(trustedAgentMarker); err != nil {
		t.Fatalf("trusted Agent credential helper did not run: %v", err)
	}
	for name, marker := range map[string]string{"Git selected through poisoned PATH": poisonGitMarker, "Agent helper selected through poisoned PATH": poisonAgentMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if strings.Contains(logs.String(), repositoryToken) {
		t.Fatalf("checkout exposed repository token in logs: %q", logs.String())
	}
}

func TestRepositoryProviderCheckoutRequiresPreResolvedGit(t *testing.T) {
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: strings.Repeat("a", 40)},
		RequiredCapabilities: []string{"provider-token-read"},
	}
	credentials := &AgentRepositoryCredentials{Agent: "/usr/bin/buildkite-agent", JobID: testCacheJobID, JobToken: "job-secret"}
	if _, err := (Runner{Git: "git", RepositoryCredentials: credentials}).runCheckout(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), job, nil); err == nil || !strings.Contains(err.Error(), "resolved before workflow execution") {
		t.Fatalf("runCheckout() unresolved Git error = %v", err)
	}
}

func TestRepositoryProviderCredentialsRejectInvalidJobIdentity(t *testing.T) {
	for name, credentials := range map[string]*AgentRepositoryCredentials{
		"missing job ID": {JobToken: "job-secret"},
		"missing token":  {JobID: testCacheJobID},
		"unsafe token":   {JobID: testCacheJobID, JobToken: "secret\nheader"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveAgentRepositoryCredentialsBeforeWorkflow(credentials); err == nil {
				t.Fatalf("resolveAgentRepositoryCredentialsBeforeWorkflow(%#v) succeeded", credentials)
			}
		})
	}
}

func TestProviderTokenReadRuntimeAuthorityIsCheckoutOnly(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/authority.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: authority\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "ordinary", Kind: "run", Command: "true"}})
	job.RequiredCapabilities = []string{"provider-token-read"}
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "restricted to the verified checkout adapter") {
		t.Fatalf("ordinary provider-token-read error = %v", err)
	}
}

func TestProviderTokenReadPreflightRejectsAnyUnknownCheckoutCommit(t *testing.T) {
	validID, parentID, unknownID := "a-0000000000000001", "a-0000000000000002", "a-0000000000000003"
	job := plan.Job{
		Steps: []plan.Step{
			{Kind: "uses", Action: &plan.ActionSelector{Lock: validID}},
			{Kind: "uses", Action: &plan.ActionSelector{Lock: parentID}},
		},
		Actions: []plan.ActionLock{
			{ID: validID, Source: "github", Repository: "actions/checkout", Commit: actionintegration.CheckoutV7Commit},
			{ID: parentID, Source: "github", Repository: "owner/composite", Commit: strings.Repeat("b", 40), Children: map[string]plan.ActionSelector{"actions/checkout@future": {Lock: unknownID}}},
			{ID: unknownID, Source: "github", Repository: "actions/checkout", Commit: strings.Repeat("0", 40)},
		},
	}
	if found, err := validateJobCheckoutAdapters(job); err == nil || found || !strings.Contains(err.Error(), "does not admit") {
		t.Fatalf("validateJobCheckoutAdapters() = %t, %v, want fail-closed rejection", found, err)
	}
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
