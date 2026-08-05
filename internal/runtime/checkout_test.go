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
