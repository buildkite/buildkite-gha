package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

type checkoutTokenProviderFunc func(context.Context, string) (string, error)

func (f checkoutTokenProviderFunc) Token(ctx context.Context, repository string) (string, error) {
	return f(ctx, repository)
}

func TestTokenlessCheckoutAdapterPopulatesVerifiedWorkspace(t *testing.T) {
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
		RequiredCapabilities: []string{"network"},
		Steps: []plan.Step{
			{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: checkoutID}},
			{ID: "local", Kind: "uses", Uses: "./.github/actions/local", Action: &plan.ActionSelector{Lock: localID}},
		},
		Actions: []plan.ActionLock{
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: strings.Repeat("b", 40), SourceDigest: remoteDigest},
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
		"fetch --no-tags --no-recurse-submodules --depth=1 origin " + sha,
		"checkout --detach " + sha,
	} {
		if !strings.Contains(log, required) {
			t.Fatalf("Git log lacks %q:\n%s", required, log)
		}
	}
	if strings.Contains(strings.ToLower(log), "authorization") || strings.Contains(strings.ToLower(log), "token") {
		t.Fatalf("Git log contains credential material: %s", log)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, ".git", "HEAD")); err != nil || strings.TrimSpace(string(got)) != sha {
		t.Fatalf("checkout HEAD = %q, %v", got, err)
	}

	job.Actions[0].SourceDigest = "sha256:" + strings.Repeat("0", 64)
	secondWorkspace := t.TempDir()
	secondLog := filepath.Join(filepath.Dir(gitLog), "tampered.log")
	if err := os.Rename(gitLog, secondLog); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Git: git, Actions: materializer}).RunJob(context.Background(), job, secondWorkspace); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered checkout lock error = %v", err)
	}
	if _, err := os.Stat(gitLog); !os.IsNotExist(err) {
		t.Fatalf("Git ran before tampered lock rejection: %v", err)
	}
}

func TestTokenlessCheckoutAdapterRejectsUnsupportedInputsAndState(t *testing.T) {
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

func TestTokenlessCheckoutPreservesPublicRepositoryValidation(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "owner/..", SHA: strings.Repeat("a", 40)}}
	processor := newCommandProcessor(io.Discard, io.Discard)
	if _, err := (Runner{}).runCheckout(context.Background(), processor, workspace, job, nil); err == nil || !strings.Contains(err.Error(), "empty workspace") {
		t.Fatalf("tokenless checkout repository validation error = %v", err)
	}
	job.RequiredCapabilities = []string{"provider-token-read"}
	if _, err := (Runner{}).runCheckout(context.Background(), processor, workspace, job, nil); err == nil || !strings.Contains(err.Error(), "valid github.com event repository") {
		t.Fatalf("private checkout repository validation error = %v", err)
	}
}

func TestPrivateCheckoutAdapterUsesOneShotAskpassCredential(t *testing.T) {
	workspace := t.TempDir()
	sha := strings.Repeat("a", 40)
	token := "ghs_private_checkout_secret"
	gitLog := filepath.Join(t.TempDir(), "git.log")
	askpassLog := filepath.Join(t.TempDir(), "askpass.log")
	git := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
case "$*" in *` + token + `*) exit 30 ;; esac
operation=
for argument in "$@"; do
  case "$argument" in init|remote|fetch|checkout) operation="$argument"; break ;; esac
done
case "$operation" in
  init)
    test -z "$GIT_ASKPASS"
    mkdir -p .git
    ;;
  remote)
    test -z "$GIT_ASKPASS"
    ;;
  fetch)
    test -n "$GIT_ASKPASS"
    if env | grep -F ` + shellTestQuote(token) + ` >/dev/null; then exit 31; fi
    printf '%s' "$GIT_ASKPASS" > ` + shellTestQuote(askpassLog) + `
    test "$("$GIT_ASKPASS" 'Username for https://github.com')" = x-access-token
    test "$("$GIT_ASKPASS" 'Password for https://github.com')" = ` + shellTestQuote(token) + `
    test -z "$("$GIT_ASKPASS" 'Password for https://github.com')"
    ;;
  checkout)
    test -z "$GIT_ASKPASS"
    printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD
    ;;
esac
printf '%s\n' "$*" >> ` + shellTestQuote(gitLog) + `
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var repositories []string
	provider := checkoutTokenProviderFunc(func(_ context.Context, repository string) (string, error) {
		repositories = append(repositories, repository)
		return token, nil
	})
	redactor := &testRedactor{}
	var logs bytes.Buffer
	processor := newCommandProcessor(&logs, &logs)
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	result, err := (Runner{Git: git, Checkout: provider, Redactor: redactor, Stdout: &logs, Stderr: &logs}).runCheckout(context.Background(), processor, workspace, job, nil)
	if err != nil {
		t.Fatalf("runCheckout() error = %v, logs = %q", err, logs.String())
	}
	if result.Outputs["commit"] != sha || result.Outputs["ref"] != job.Event.Ref {
		t.Fatalf("checkout outputs = %#v", result.Outputs)
	}
	if len(repositories) != 1 || repositories[0] != job.Event.Repository || len(redactor.values) != 1 || redactor.values[0] != token {
		t.Fatalf("repositories/redactions = %#v / %#v", repositories, redactor.values)
	}
	gitBytes, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gitBytes), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("checkout exposed token in Git arguments or logs: %q / %q", gitBytes, logs.String())
	}
	helperBytes, err := os.ReadFile(askpassLog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(helperBytes)); !os.IsNotExist(err) {
		t.Fatalf("askpass helper survived checkout: %v", err)
	}
	configBytes, err := os.ReadFile(filepath.Join(workspace, ".git", "config"))
	if err == nil && strings.Contains(string(configBytes), token) {
		t.Fatalf("Git config contains checkout token: %q", configBytes)
	}
}

func TestPrivateCheckoutPinsGitBeforeActionPreHooks(t *testing.T) {
	workspace := t.TempDir()
	workflowSource := []byte("name: private checkout executable confinement\n")
	sha := strings.Repeat("a", 40)
	token := "ghs_private_checkout_secret"

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
    test "$("$GIT_ASKPASS" 'Username for https://github.com')" = x-access-token
    test "$("$GIT_ASKPASS" 'Password for https://github.com')" = ` + shellTestQuote(token) + `
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
	trustedAgentScript := "#!/bin/sh\nIFS= read -r value\ntest \"$value\" = " + shellTestQuote(token) + "\n: > " + shellTestQuote(trustedAgentMarker) + "\n"
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
	poisonDir := t.TempDir()
	poisonGitMarker := filepath.Join(t.TempDir(), "poison-git-ran")
	poisonGit := filepath.Join(poisonDir, "git")
	if err := os.WriteFile(poisonGit, []byte("#!/bin/sh\ntouch "+shellTestQuote(poisonGitMarker)+"\nexit 97\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	poisonCatMarker := filepath.Join(t.TempDir(), "poison-cat-ran")
	poisonCat := filepath.Join(poisonDir, "cat")
	if err := os.WriteFile(poisonCat, []byte("#!/bin/sh\ntouch "+shellTestQuote(poisonCatMarker)+"\nexit 98\n"), 0o700); err != nil {
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
  ln -s "$POISON_CAT" "$LOOKUP_CAT"
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
			"LOOKUP_CAT":   filepath.Join(lookupDir, "cat"),
			"POISON_CAT":   poisonCat,
			"POISON_GIT":   poisonGit,
		},
		Steps: []plan.Step{
			{ID: "poison", Kind: "uses", Uses: "owner/repo/poison@v1", Action: &plan.ActionSelector{Lock: poisonID}},
			{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: checkoutID}},
		},
		Actions: []plan.ActionLock{
			{ID: poisonID, Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: strings.Repeat("b", 40), Path: "poison", SourceDigest: remoteDigest},
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: strings.Repeat("c", 40), SourceDigest: remoteDigest},
		},
	}
	provider := checkoutTokenProviderFunc(func(context.Context, string) (string, error) { return token, nil })
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: remoteDigest}}
	var logs bytes.Buffer
	result, err := (Runner{Node24: node, Checkout: provider, Redactor: AgentRedactor{}, Actions: materializer, Stdout: &logs, Stderr: &logs}).RunJob(context.Background(), job, workspace)
	if err != nil || result.Conclusion != "success" {
		t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
	}
	if _, err := os.Stat(trustedLog); err != nil {
		t.Fatalf("trusted Git did not run: %v", err)
	}
	if target, err := filepath.EvalSymlinks(lookupGit); err != nil || target != poisonGit {
		t.Fatalf("pre-hook Git replacement = %q, %v; want %q", target, err, poisonGit)
	}
	if target, err := filepath.EvalSymlinks(filepath.Join(lookupDir, "cat")); err != nil || target != poisonCat {
		t.Fatalf("pre-hook cat replacement = %q, %v; want %q", target, err, poisonCat)
	}
	if target, err := filepath.EvalSymlinks(lookupAgent); err != nil || target != poisonAgent {
		t.Fatalf("pre-hook Agent replacement = %q, %v; want %q", target, err, poisonAgent)
	}
	if _, err := os.Stat(trustedAgentMarker); err != nil {
		t.Fatalf("trusted Agent redactor did not run: %v", err)
	}
	for name, marker := range map[string]string{"Git selected through poisoned PATH": poisonGitMarker, "askpass reader selected through poisoned PATH": poisonCatMarker, "Agent redactor selected through poisoned PATH": poisonAgentMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("checkout exposed token in logs: %q", logs.String())
	}
}

func TestPrivateCheckoutRequiresPreResolvedGit(t *testing.T) {
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: strings.Repeat("a", 40)},
		RequiredCapabilities: []string{"provider-token-read"},
	}
	if _, err := (Runner{Git: "git"}).runCheckout(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), job, nil); err == nil || !strings.Contains(err.Error(), "resolved before workflow execution") {
		t.Fatalf("runCheckout() unresolved Git error = %v", err)
	}
}

func TestPrivateCheckoutRedactorFailureAbortsBeforeFetchAndScrubsToken(t *testing.T) {
	token := "ghs_private_checkout_secret"
	marker := filepath.Join(t.TempDir(), "fetch-ran")
	git := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\ncase \"$*\" in *fetch*) touch " + shellTestQuote(marker) + " ;; init*) mkdir -p .git ;; esac\n"
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: strings.Repeat("a", 40)},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	provider := checkoutTokenProviderFunc(func(context.Context, string) (string, error) { return token, nil })
	err := error(nil)
	_, err = (Runner{Git: git, Checkout: provider, Redactor: failingCacheRedactor{token: token}}).runCheckout(context.Background(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), job, nil)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("runCheckout() error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Git fetch ran before redactor registration succeeded: %v", err)
	}
}

func TestProviderTokenReadRuntimeAuthorityIsCheckoutOnly(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/authority.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: authority\n")
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "ordinary", Kind: "run", Command: "true"}})
	job.RequiredCapabilities = []string{"provider-token-read"}
	if _, err := (Runner{}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "requires the private checkout token provider") {
		t.Fatalf("missing provider error = %v", err)
	}
	provider := checkoutTokenProviderFunc(func(context.Context, string) (string, error) { return "ghs_unused", nil })
	if _, err := (Runner{Checkout: provider}).RunJob(context.Background(), job, workspace); err == nil || !strings.Contains(err.Error(), "restricted to the verified checkout adapter") {
		t.Fatalf("ordinary provider-token-read error = %v", err)
	}
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
