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
	if strings.Contains(strings.ToLower(log), "authorization") || strings.Contains(strings.ToLower(log), "token") || strings.Contains(log, "git-credentials-helper") {
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
    mkdir -p .git
    ;;
  remote)
    test -z "$GIT_ASKPASS"
    test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
    test -z "${BUILDKITE_JOB_ID+x}"
    test -z "${BUILDKITE_NO_HTTP2+x}"
    ;;
  fetch)
    test -z "$GIT_ASKPASS"
    test "$BUILDKITE_AGENT_ENDPOINT" = https://agent.example/v3
    test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
    test "$BUILDKITE_JOB_ID" = 11111111-1111-4111-8111-111111111111
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
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: strings.Repeat("c", 40), SourceDigest: remoteDigest},
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

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
