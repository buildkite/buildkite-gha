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
	"maps"
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
test "$GIT_CONFIG_GLOBAL" = ` + shellTestQuote(os.DevNull) + `
test "$GIT_TERMINAL_PROMPT" = 0
test "$GIT_LFS_SKIP_SMUDGE" = 1
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
    printf '%s\n' '[url "https://attacker.invalid/"]' '  insteadOf = https://github.com/' > .no-global-gitconfig
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(workflowSource)) + ` | base64 -d > .github/workflows/test.yml
    printf '%s' ` + shellTestQuote(base64.StdEncoding.EncodeToString(localSource)) + ` | base64 -d > .github/actions/local/action.yml
    chmod 0644 .no-global-gitconfig .github/workflows/test.yml .github/actions/local/action.yml
    ;;
esac
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	workflowDigest := sha256.Sum256(workflowSource)
	headDigest := githubHash(sha + "\n")
	checkoutID, localID := "a-0000000000000001", "a-0000000000000002"
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version: "checkout-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Runtime: &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
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
			{ID: "hash", Kind: "run", Shell: "sh", Condition: "hashFiles('.git/HEAD') != ''", Env: map[string]string{"HEAD_HASH": "${{ hashFiles('.git/HEAD') }}"}, Command: "test \"$HEAD_HASH\" = " + headDigest},
			{ID: "local", Kind: "uses", Uses: "./.github/actions/local", Action: &plan.ActionSelector{Lock: localID}},
		},
		Actions: []plan.ActionLock{
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: remoteDigest},
			{ID: localID, Source: "workspace", Path: ".github/actions/local", SourceDigest: localDigest},
		},
		RequiresMise: &requiresMise,
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}
	result, err := (Runner{Git: git, Actions: materializer}).RunJob(t.Context(), job, workspace)
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

	job.Steps[0].With = map[string]string{"ref": "${{ needs.configure.outputs.sha }}"}
	job.Needs = map[string]plan.Need{"configure": {Result: "success", Outputs: map[string]string{"sha": strings.Repeat("b", 40)}}}
	if err := os.Remove(gitLog); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Git: git, Actions: materializer}).RunJob(t.Context(), job, t.TempDir()); err == nil || !strings.Contains(err.Error(), "dynamic ref must resolve to the exact event SHA") {
		t.Fatalf("dynamic checkout ref error = %v", err)
	}
	if _, err := os.Stat(gitLog); !os.IsNotExist(err) {
		t.Fatalf("Git ran before dynamic checkout ref rejection: %v", err)
	}
	job.Steps[0].With = nil
	job.Needs = nil

	job.Actions[0].Commit = strings.Repeat("0", 40)
	unknownWorkspace := t.TempDir()
	if _, err := (Runner{Git: git, Actions: materializer}).RunJob(t.Context(), job, unknownWorkspace); err == nil || !strings.Contains(err.Error(), "does not admit") {
		t.Fatalf("unknown checkout commit error = %v", err)
	}
	if _, err := os.Stat(gitLog); !os.IsNotExist(err) {
		t.Fatalf("Git ran before unknown checkout commit rejection: %v", err)
	}

	job.Actions[0].Commit = actionintegration.CheckoutV7Commit
	job.Actions[0].SourceDigest = "sha256:" + strings.Repeat("0", 64)
	secondWorkspace := t.TempDir()
	if _, err := (Runner{Git: git, Actions: materializer}).RunJob(t.Context(), job, secondWorkspace); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered checkout lock error = %v", err)
	}
	if _, err := os.Stat(gitLog); !os.IsNotExist(err) {
		t.Fatalf("Git ran before tampered lock rejection: %v", err)
	}
}

func TestCheckoutRepositoryURL(t *testing.T) {
	for _, test := range []struct {
		provider, repository, wantURL, wantHost string
		wantOK                                  bool
	}{
		{provider: "github", repository: "acme/widgets", wantURL: "https://github.com/acme/widgets.git", wantHost: "github.com", wantOK: true},
		{provider: "cursor-origin", repository: "acme/widgets", wantURL: "https://origin.cursor.com/git/acme/widgets.git", wantHost: "origin.cursor.com", wantOK: true},
		{provider: "other", repository: "acme/widgets"},
		{provider: "cursor-origin", repository: "acme/widgets/extra"},
	} {
		t.Run(test.provider+"/"+test.repository, func(t *testing.T) {
			url, host, ok := checkoutRepositoryURL(test.provider, test.repository)
			if url != test.wantURL || host != test.wantHost || ok != test.wantOK {
				t.Fatalf("checkoutRepositoryURL() = %q, %q, %t", url, host, ok)
			}
		})
	}
}

func TestRepositoryProviderCheckoutCredentialArgsUseProviderHost(t *testing.T) {
	args := strings.Join(repositoryProviderCheckoutCredentialArgs([]string{"git"}, "/usr/bin/buildkite-agent", "origin.cursor.com"), "\n")
	if !strings.Contains(args, "credential.https://origin.cursor.com.useHttpPath=true") ||
		!strings.Contains(args, "credential.https://origin.cursor.com.helper=") || strings.Contains(args, "credential.https://github.com") {
		t.Fatalf("Origin credential arguments = %q", args)
	}
}

func TestOriginCheckoutUsesExactRemoteAndCredentialHost(t *testing.T) {
	workspace := t.TempDir()
	sha := strings.Repeat("a", 40)
	gitLog := filepath.Join(t.TempDir(), "git.log")
	helperInput := filepath.Join(t.TempDir(), "helper-input")
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	agentScript := `#!/bin/sh
set -eu
test "$1" = git-credentials-helper
test "$2" = get
cat > ` + shellTestQuote(helperInput) + `
printf 'username=token\npassword=origin-repository-token\n'
`
	if err := os.WriteFile(agent, []byte(agentScript), 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(t.TempDir(), "git")
	gitScript := `#!/bin/sh
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
      case "$argument" in
        credential.https://origin.cursor.com.helper=!*) helper="${argument#credential.https://origin.cursor.com.helper=!}" ;;
        credential.https://github.com.*) exit 42 ;;
      esac
    done
    test -n "$helper"
    credentials="$(printf 'protocol=https\nhost=origin.cursor.com\npath=git/acme/widgets.git\n\n' | sh -c "$helper get")"
    case "$credentials" in
      *"username=token"*"password=origin-repository-token"*) ;;
      *) exit 43 ;;
    esac
    ;;
  checkout) printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD ;;
esac
printf '%s\n' "$*" >> ` + shellTestQuote(gitLog) + `
`
	if err := os.WriteFile(git, []byte(gitScript), 0o700); err != nil {
		t.Fatal(err)
	}
	credentials, err := resolveAgentRepositoryCredentialsBeforeWorkflow(&AgentRepositoryCredentials{
		Agent: agent, JobID: testCacheJobID, JobToken: "job-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := plan.Job{
		Event:                plan.Event{Provider: "cursor-origin", Repository: "acme/widgets", Ref: "refs/heads/main", SHA: sha},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	var logs bytes.Buffer
	result, err := (Runner{Git: git, RepositoryCredentials: credentials, Stdout: &logs, Stderr: &logs}).runCheckout(
		t.Context(), newCommandProcessor(&logs, &logs), workspace, job, actionintegration.CheckoutV7Commit, nil,
	)
	if err != nil {
		t.Fatalf("runCheckout() error = %v, logs = %q", err, logs.String())
	}
	if result.Outputs["commit"] != sha {
		t.Fatalf("checkout outputs = %#v", result.Outputs)
	}
	gitCommands, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gitCommands), "remote add origin https://origin.cursor.com/git/acme/widgets.git") ||
		!strings.Contains(string(gitCommands), "credential.https://origin.cursor.com.useHttpPath=true") ||
		strings.Contains(string(gitCommands), "credential.https://github.com") {
		t.Fatalf("Origin Git commands = %q", gitCommands)
	}
	input, err := os.ReadFile(helperInput)
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != "protocol=https\nhost=origin.cursor.com\npath=git/acme/widgets.git\n\n" {
		t.Fatalf("Origin credential input = %q", input)
	}
}

func TestCheckoutAdapterRejectsUnsupportedInputsAndState(t *testing.T) {
	repository, sha := "buildkite/buildkite-gha", strings.Repeat("a", 40)
	processor := newCommandProcessor(io.Discard, io.Discard)
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: repository, SHA: sha}}
	if _, err := (Runner{}).runCheckout(t.Context(), processor, t.TempDir(), job, actionintegration.CheckoutV7Commit, map[string]string{"token": ""}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("runCheckout() unsupported input error = %v", err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{}).runCheckout(t.Context(), processor, workspace, job, actionintegration.CheckoutV7Commit, nil); err == nil || !strings.Contains(err.Error(), "empty workspace") {
		t.Fatalf("nonempty workspace error = %v", err)
	}
	job.Event.Provider = "other"
	if _, err := (Runner{}).runCheckout(t.Context(), processor, t.TempDir(), job, actionintegration.CheckoutV7Commit, nil); err == nil || !strings.Contains(err.Error(), "valid GitHub or Origin event") {
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
		{name: "explicit history", inputs: map[string]string{"fetch-depth": "100"}, want: "fetch --no-tags --no-recurse-submodules --progress --depth=100 origin " + sha},
		{name: "other commit", inputs: map[string]string{"ref": strings.Repeat("b", 40)}, want: "fetch --no-tags --no-recurse-submodules --progress --depth=1 origin " + strings.Repeat("b", 40)},
		{name: "shallow tags", inputs: map[string]string{"fetch-tags": "TRUE"}, want: "fetch --no-tags --no-recurse-submodules --progress --depth=1 origin " + sha + " +refs/tags/*:refs/tags/*"},
		{name: "progress disabled", inputs: map[string]string{"show-progress": "false"}, want: "fetch --no-tags --no-recurse-submodules --depth=1 origin " + sha},
		{
			name: "bounded branch", inputs: map[string]string{"ref": "test-catalog", "fetch-depth": "100"},
			want: "fetch --no-tags --no-recurse-submodules --progress --depth=100 origin +refs/heads/test-catalog:refs/remotes/origin/test-catalog",
		},
		{
			name: "all branches and tags", inputs: map[string]string{"Fetch-Depth": "0"},
			want: "fetch --no-tags --no-recurse-submodules --progress --prune origin +refs/heads/*:refs/remotes/origin/* +refs/tags/*:refs/tags/* +" + sha + ":refs/buildkite-gha/event",
		},
		{
			name: "all branches for explicit branch", inputs: map[string]string{"ref": "refs/heads/test-catalog", "fetch-depth": "0"},
			want: "fetch --no-tags --no-recurse-submodules --progress --prune origin +refs/heads/*:refs/remotes/origin/* +refs/tags/*:refs/tags/*",
		},
		{
			name: "all history for explicit commit", inputs: map[string]string{"ref": strings.Repeat("b", 40), "fetch-depth": "0"},
			want: "fetch --no-tags --no-recurse-submodules --progress --prune origin +refs/heads/*:refs/remotes/origin/* +refs/tags/*:refs/tags/* +" + strings.Repeat("b", 40) + ":refs/buildkite-gha/selected",
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
		{name: "branch", inputs: map[string]string{"ref": "test-catalog"}, want: "test-catalog"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := checkoutRefOutput(test.inputs, eventRef); got != test.want {
				t.Fatalf("checkoutRefOutput() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCheckoutOutputsMatchReleaseContract(t *testing.T) {
	for _, test := range []struct {
		name   string
		commit string
		want   map[string]string
	}{
		{name: "v1.2.0", commit: actionintegration.CheckoutV1Commit, want: map[string]string{}},
		{name: "v2.8.0", commit: actionintegration.CheckoutV2Commit, want: map[string]string{}},
		{name: "v3.7.0", commit: actionintegration.CheckoutV3Commit, want: map[string]string{}},
		{name: "v4 and later", commit: actionintegration.CheckoutV4Commit, want: map[string]string{"ref": "refs/heads/main", "commit": strings.Repeat("a", 40)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			outputs := map[string]string{}
			setCheckoutOutputs(outputs, test.commit, "refs/heads/main", strings.Repeat("a", 40))
			if !maps.Equal(outputs, test.want) {
				t.Fatalf("checkout outputs = %#v, want %#v", outputs, test.want)
			}
		})
	}
}

func TestCheckoutInputsWithReleaseDefaults(t *testing.T) {
	for _, test := range []struct {
		name   string
		commit string
		inputs map[string]string
		want   map[string]string
	}{
		{name: "v1 defaults to full history", commit: actionintegration.CheckoutV1Commit, inputs: nil, want: map[string]string{"fetch-depth": "0"}},
		{name: "v1 keeps explicit depth", commit: actionintegration.CheckoutV1Commit, inputs: map[string]string{"Fetch-Depth": "5"}, want: map[string]string{"Fetch-Depth": "5"}},
		{name: "v2 keeps shallow default", commit: actionintegration.CheckoutV2Commit, inputs: nil, want: nil},
		{name: "v4 keeps shallow default", commit: actionintegration.CheckoutV4Commit, inputs: map[string]string{"ref": "main"}, want: map[string]string{"ref": "main"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := checkoutInputsWithReleaseDefaults(test.commit, test.inputs); !maps.Equal(got, test.want) {
				t.Fatalf("checkoutInputsWithReleaseDefaults() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestCheckoutSubmoduleInputMode(t *testing.T) {
	if got := checkoutSubmoduleMode(map[string]string{"SuBmOdUlEs": " ReCuRsIvE "}); got != "recursive" {
		t.Fatalf("checkoutSubmoduleMode() = %q", got)
	}
}

func TestCheckoutLFSFiltersPinExecutable(t *testing.T) {
	gitLFS := "/opt/pinned lfs/git-lfs'quoted"
	filterArgs := strings.Join(checkoutGitLFSFilterArgs(checkoutGitBaseArgs(), gitLFS), "\n")
	if !strings.Contains(filterArgs, "core.hooksPath=/dev/null") || !strings.Contains(filterArgs, `'/opt/pinned lfs/git-lfs'\''quoted' filter-process`) || strings.Contains(filterArgs, "filter.lfs.process=git-lfs") {
		t.Fatalf("checkout LFS filter arguments = %q", filterArgs)
	}
}

func TestCheckoutLFSSubmodulesPinExecutable(t *testing.T) {
	workspace := canonicalTempDir(t)
	gitLog := filepath.Join(t.TempDir(), "git.log")
	sha := strings.Repeat("a", 40)
	git := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> ` + shellTestQuote(gitLog) + `
case "$*" in
  *" init --template= .") mkdir -p .git ;;
  *" checkout --detach "*) printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD ;;
esac
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitLFS := filepath.Join(t.TempDir(), "git-lfs")
	if err := os.WriteFile(gitLFS, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: sha}}
	if _, err := (Runner{Git: git, GitLFS: gitLFS}).runCheckout(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, job, actionintegration.CheckoutV7Commit, map[string]string{"lfs": "true", "submodules": "true"}); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(commands)), "\n") {
		if strings.Contains(line, "submodule ") && !strings.Contains(line, "filter.lfs.process="+checkoutGitLFSExecutable(gitLFS)+" filter-process") {
			t.Fatalf("submodule command did not pin Git LFS: %q", line)
		}
	}
}

func TestCheckoutGitConfigEnvironment(t *testing.T) {
	env := checkoutGitConfigEnvironment(map[string]string{"EXISTING": "value"}, []string{"--literal-pathspecs", "-c", "credential.helper=", "-c", "http.followRedirects=false"})
	if env["EXISTING"] != "value" || env["GIT_CONFIG_COUNT"] != "2" || env["GIT_CONFIG_KEY_0"] != "credential.helper" || env["GIT_CONFIG_VALUE_0"] != "" || env["GIT_CONFIG_KEY_1"] != "http.followRedirects" || env["GIT_CONFIG_VALUE_1"] != "false" {
		t.Fatalf("Git config environment = %#v", env)
	}
}

func TestCheckoutSubmoduleNativeCommandSequenceAndFlags(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "git.log")
	git := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellTestQuote(logPath) + "\n"
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := Runner{Stdout: io.Discard, Stderr: io.Discard}
	if err := runner.runCheckoutSubmodules(t.Context(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), git, map[string]string{}, checkoutGitBaseArgs(), "7", true, false, ""); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "submodule sync --recursive") || !strings.Contains(lines[1], "submodule update --init --force --depth=7 --recursive") || !strings.Contains(lines[2], "submodule status --recursive") {
		t.Fatalf("native submodule command sequence = %q", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "url.https://github.com/.insteadOf=git@github.com:") {
			t.Fatalf("command lacks scoped GitHub rewrite: %q", line)
		}
		if strings.Contains(line, "credential.helper=!") {
			t.Fatalf("command contains generic credential helper: %q", line)
		}
	}
}

func TestCheckoutSubmoduleStatusRejectsInvalidStates(t *testing.T) {
	for _, prefix := range []string{"-", "+", "U"} {
		t.Run(prefix, func(t *testing.T) {
			git := filepath.Join(t.TempDir(), "git")
			script := `#!/bin/sh
previous=
for argument in "$@"; do
  if [ "$previous" = submodule ] && [ "$argument" = status ]; then
    printf '%s%s child\n' ` + shellTestQuote(prefix) + ` ` + shellTestQuote(strings.Repeat("a", 40)) + `
    exit 0
  fi
  previous="$argument"
done
exit 0
`
			if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			err := (Runner{Stdout: io.Discard, Stderr: io.Discard}).runCheckoutSubmodules(t.Context(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), git, map[string]string{}, checkoutGitBaseArgs(), "0", false, false, "")
			if err == nil || !strings.Contains(err.Error(), "invalid state") {
				t.Fatalf("status prefix %q error = %v", prefix, err)
			}
		})
	}
}

func TestCheckoutSubmodulesUsesNativePorcelain(t *testing.T) {
	root := t.TempDir()
	_, grandOID := createNativeSubmoduleRepository(t, root, "grand", "grand.txt", "grand\n", "", "")
	_, childOID := createNativeSubmoduleRepository(t, root, "child", "child.txt", "child\n", "../grand.git", "deps/grand")
	_, parentOID := createNativeSubmoduleRepository(t, root, "parent", "parent.txt", "parent\n", "../child.git", "deps/child")

	for _, test := range []struct {
		name      string
		recursive bool
		depthOne  bool
	}{
		{name: "direct depth zero"},
		{name: "recursive depth one", recursive: true, depthOne: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "parent")
			runTestGit(t, "", "clone", filepath.Join(root, "parent.git"), workspace)
			runTestGit(t, workspace, "checkout", "--detach", parentOID)
			base := append(checkoutGitBaseArgs(), "-c", "protocol.file.allow=always")
			runner := Runner{Stdout: io.Discard, Stderr: io.Discard}
			depth := "0"
			if test.depthOne {
				depth = "1"
			}
			if err := runner.runCheckoutSubmodules(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, "git", map[string]string{"HOME": filepath.Join(workspace, ".no-home"), "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull}, base, depth, test.recursive, false, ""); err != nil {
				t.Fatal(err)
			}
			childPath := filepath.Join(workspace, "deps", "child")
			if got := strings.TrimSpace(runTestGit(t, childPath, "rev-parse", "HEAD")); got != childOID {
				t.Fatalf("child HEAD = %s, want %s", got, childOID)
			}
			if _, err := os.Stat(filepath.Join(workspace, ".git", "modules", "deps", "child", "HEAD")); err != nil {
				t.Fatalf("native child module layout: %v", err)
			}
			grandPath := filepath.Join(childPath, "deps", "grand")
			if test.recursive {
				if got := strings.TrimSpace(runTestGit(t, grandPath, "rev-parse", "HEAD")); got != grandOID {
					t.Fatalf("grandchild HEAD = %s, want %s", got, grandOID)
				}
				if _, err := os.Stat(filepath.Join(workspace, ".git", "modules", "deps", "child", "modules", "deps", "grand", "HEAD")); err != nil {
					t.Fatalf("native nested module layout: %v", err)
				}
			} else if _, err := os.Stat(filepath.Join(grandPath, ".git")); !os.IsNotExist(err) {
				t.Fatalf("direct mode initialized nested child: %v", err)
			}
			statusArgs := []string{"submodule", "status"}
			if test.recursive {
				statusArgs = append(statusArgs, "--recursive")
			}
			for line := range strings.SplitSeq(strings.TrimSpace(runTestGit(t, workspace, statusArgs...)), "\n") {
				if line == "" || line[0] == '-' {
					t.Fatalf("uninitialized status %q", line)
				}
			}
		})
	}
}

func createNativeSubmoduleRepository(t *testing.T, root, name, file, contents, childURL, childPath string) (string, string) {
	t.Helper()
	work := filepath.Join(root, name+"-work")
	runTestGit(t, "", "init", "--initial-branch=main", work)
	runTestGit(t, work, "config", "user.name", "buildkite-gha test")
	runTestGit(t, work, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(work, file), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, work, "add", file)
	if childURL != "" {
		runTestGit(t, work, "-c", "protocol.file.allow=always", "submodule", "add", childURL, childPath)
	}
	runTestGit(t, work, "commit", "-m", "fixture")
	oid := strings.TrimSpace(runTestGit(t, work, "rev-parse", "HEAD"))
	bare := filepath.Join(root, name+".git")
	runTestGit(t, "", "clone", "--bare", work, bare)
	return bare, oid
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
	runner := Runner{RepositoryCredentials: &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "secret"}, Stdout: io.Discard, Stderr: io.Discard}
	if err := runner.runCheckoutSubmodules(t.Context(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), git, map[string]string{}, checkoutGitBaseArgs(), "0", false, false, ""); err != nil {
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
  case "$argument" in credential.https://github.com.helper=!*) helper="${argument#credential.https://github.com.helper=!}" ;; esac
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
	runner := Runner{RepositoryCredentials: &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "job-secret"}, Stdout: &logs, Stderr: &logs}
	if err := runner.runRepositoryProviderCheckoutGit(t.Context(), newCommandProcessor(&logs, &logs), workspace, map[string]string{}, git, checkoutGitBaseArgs(), []string{"submodule", "update"}, "github.com"); err != nil {
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
	runTestGit(t, sourceRoot, "branch", "test-catalog", commits[1])
	remote := filepath.Join(t.TempDir(), "remote.git")
	runTestGit(t, "", "clone", "--bare", sourceRoot, remote)

	for _, test := range []struct {
		name        string
		inputs      map[string]string
		wantCount   string
		wantTagTip  bool
		otherCommit bool
	}{
		{name: "depth one exact detached SHA", wantCount: "1"},
		{name: "bounded history", inputs: map[string]string{"fetch-depth": "100", "show-progress": "false"}, wantCount: "2"},
		{name: "depth zero history and tags", inputs: map[string]string{"fetch-depth": "0", "show-progress": "false"}, wantCount: "2", wantTagTip: true},
		{name: "shallow explicit tags", inputs: map[string]string{"fetch-tags": "true", "show-progress": "false"}, wantCount: "1", wantTagTip: true},
		{name: "bounded branch", inputs: map[string]string{"ref": "test-catalog", "fetch-depth": "100", "show-progress": "false"}, wantCount: "2"},
		{name: "other exact commit", inputs: map[string]string{"show-progress": "false"}, wantCount: "1", otherCommit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			eventSHA, wantSHA := commits[1], commits[1]
			inputs := test.inputs
			if test.otherCommit {
				wantSHA = commits[0]
				inputs = map[string]string{"ref": wantSHA, "show-progress": "false"}
			}
			workspace := t.TempDir()
			runTestGit(t, workspace, "init", "--initial-branch=main")
			runTestGit(t, workspace, "remote", "add", "origin", remote)
			runTestGit(t, workspace, checkoutFetchArgs(inputs, eventSHA)...)
			runTestGit(t, workspace, "checkout", "--detach", checkoutRevision(inputs, eventSHA))
			if got := strings.TrimSpace(runTestGit(t, workspace, "rev-parse", "HEAD")); got != wantSHA {
				t.Fatalf("HEAD = %s, want exact SHA %s", got, wantSHA)
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

func TestPrepareCheckoutDirectory(t *testing.T) {
	workspace := t.TempDir()
	root, err := prepareCheckoutDirectory(workspace, map[string]string{"path": "sources/test-catalog", "clean": "false"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workspace, "sources", "test-catalog")
	if root != want {
		t.Fatalf("checkout directory = %q, want %q", root, want)
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		t.Fatalf("checkout directory state = %#v, %v", info, err)
	}
	if _, err := prepareCheckoutDirectory(workspace, map[string]string{"path": "sources/test-catalog", "clean": "false"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second checkout path error = %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareCheckoutDirectory(workspace, map[string]string{"path": "linked/repository"}); err == nil || !strings.Contains(err.Error(), "symbolic-link parent") {
		t.Fatalf("symbolic-link parent error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "repository")); !os.IsNotExist(err) {
		t.Fatalf("checkout escaped through symbolic-link parent: %v", err)
	}
}

func TestCheckoutFetchArgsApplyFilterAndSparsePrecedence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name   string
		inputs map[string]string
		want   string
	}{
		{name: "explicit filter", inputs: map[string]string{"filter": "tree:0", "show-progress": "false"}, want: "fetch --no-tags --no-recurse-submodules --filter=tree:0 --depth=1 origin " + sha},
		{name: "sparse default filter", inputs: map[string]string{"sparse-checkout": "src\ndocs", "show-progress": "false"}, want: "fetch --no-tags --no-recurse-submodules --filter=blob:none --depth=1 origin " + sha},
		{name: "explicit filter overrides sparse default", inputs: map[string]string{"filter": "blob:limit=1m", "sparse-checkout": "src"}, want: "fetch --no-tags --no-recurse-submodules --progress --filter=blob:limit=1m --depth=1 origin " + sha},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := strings.Join(checkoutFetchArgs(test.inputs, sha), " "); got != test.want {
				t.Fatalf("checkoutFetchArgs() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConfigureSparseCheckoutModes(t *testing.T) {
	t.Run("cone", func(t *testing.T) {
		var commands [][]string
		err := configureSparseCheckout(t.TempDir(), map[string]string{"sparse-checkout": " src \n\ndocs"}, func(args ...string) error {
			commands = append(commands, append([]string(nil), args...))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := [][]string{{"sparse-checkout", "set", "--", "src", "docs"}}
		if !slices.EqualFunc(commands, want, slices.Equal) {
			t.Fatalf("sparse checkout commands = %#v, want %#v", commands, want)
		}
	})

	t.Run("non-cone", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		var commands [][]string
		err := configureSparseCheckout(workspace, map[string]string{"sparse-checkout": "*.go\n!vendor/", "sparse-checkout-cone-mode": "false"}, func(args ...string) error {
			commands = append(commands, append([]string(nil), args...))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := [][]string{{"config", "core.sparseCheckout", "true"}}
		if !slices.EqualFunc(commands, want, slices.Equal) {
			t.Fatalf("sparse checkout commands = %#v, want %#v", commands, want)
		}
		patterns, err := os.ReadFile(filepath.Join(workspace, ".git", "info", "sparse-checkout"))
		if err != nil || string(patterns) != "\n*.go\n!vendor/\n" {
			t.Fatalf("non-cone patterns = %q, %v", patterns, err)
		}
	})
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

func requireProcessExit(t *testing.T, pid, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("sh", "-c", "kill -0 "+pid).Run(); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s child process %s remains alive", description, pid)
}

func TestCheckoutSubmoduleCancellationCleansNativeUpdateProcessGroup(t *testing.T) {
	root := t.TempDir()
	createNativeSubmoduleRepository(t, root, "child", "child.txt", "child\n", "", "")
	_, parentOID := createNativeSubmoduleRepository(t, root, "parent", "parent.txt", "parent\n", "../child.git", "deps/child")
	workspace := filepath.Join(t.TempDir(), "parent")
	runTestGit(t, "", "clone", filepath.Join(root, "parent.git"), workspace)
	runTestGit(t, workspace, "checkout", "--detach", parentOID)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	pidFile, updateStarted := filepath.Join(t.TempDir(), "child.pid"), filepath.Join(t.TempDir(), "update-started")
	wrapper := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
previous=
for argument in "$@"; do
  if [ "$previous" = submodule ] && [ "$argument" = update ]; then
    (trap '' INT TERM; sleep 30) &
    echo $! > ` + shellTestQuote(pidFile) + `
    : > ` + shellTestQuote(updateStarted) + `
    wait
  fi
  previous="$argument"
done
exec ` + shellTestQuote(realGit) + ` "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runner := Runner{Stdout: io.Discard, Stderr: io.Discard, InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- runner.runCheckoutSubmodules(ctx, newCommandProcessor(io.Discard, io.Discard), workspace, wrapper, map[string]string{}, append(checkoutGitBaseArgs(), "-c", "protocol.file.allow=always"), "1", false, false, "")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(updateStarted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submodule update did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("submodule update cancellation error = %v", err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	requireProcessExit(t, pid, "update")
}

func TestCheckoutSubmoduleCancellationCleansNativeStatusProcessGroup(t *testing.T) {
	pidFile, statusStarted := filepath.Join(t.TempDir(), "child.pid"), filepath.Join(t.TempDir(), "status-started")
	git := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
previous=
for argument in "$@"; do
  if [ "$previous" = submodule ] && [ "$argument" = status ]; then
    (trap '' INT TERM; sleep 30) &
    echo $! > ` + shellTestQuote(pidFile) + `
    : > ` + shellTestQuote(statusStarted) + `
    wait
  fi
  previous="$argument"
done
exit 0
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runner := Runner{Stdout: io.Discard, Stderr: io.Discard, InterruptGrace: 20 * time.Millisecond, TerminateGrace: 20 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- runner.runCheckoutSubmodules(ctx, newCommandProcessor(io.Discard, io.Discard), t.TempDir(), git, map[string]string{}, checkoutGitBaseArgs(), "0", true, false, "")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(statusStarted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submodule status did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("submodule status cancellation error = %v", err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	requireProcessExit(t, pid, "status")
}

func TestCheckoutRejectsInvalidRepositoryBeforeInspectingWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "owner/..", SHA: strings.Repeat("a", 40)}}
	processor := newCommandProcessor(io.Discard, io.Discard)
	if _, err := (Runner{}).runCheckout(t.Context(), processor, workspace, job, actionintegration.CheckoutV7Commit, nil); err == nil || !strings.Contains(err.Error(), "valid GitHub or Origin event repository") {
		t.Fatalf("checkout repository validation error = %v", err)
	}
}

func TestCheckoutRejectsUnavailableLFSBeforeCreatingPath(t *testing.T) {
	workspace := t.TempDir()
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: strings.Repeat("a", 40)}}
	inputs := map[string]string{"lfs": "true", "path": "sources/application"}
	if _, err := (Runner{}).runCheckout(t.Context(), newCommandProcessor(io.Discard, io.Discard), workspace, job, actionintegration.CheckoutV7Commit, inputs); err == nil || !strings.Contains(err.Error(), "requires Git LFS to be resolved") {
		t.Fatalf("unavailable Git LFS error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "sources")); !os.IsNotExist(err) {
		t.Fatalf("unavailable Git LFS created checkout path: %v", err)
	}
}

func TestCheckoutUsesCommandScopedAgentCredentialHelper(t *testing.T) {
	workspace := canonicalTempDir(t)
	checkoutDirectory := filepath.Join(workspace, "test-catalog")
	poisonedGlobalConfig := []byte("[url \"https://attacker.invalid/\"]\n\tinsteadOf = https://github.com/\n")
	if err := os.WriteFile(filepath.Join(workspace, ".no-global-gitconfig"), poisonedGlobalConfig, 0o600); err != nil {
		t.Fatal(err)
	}
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
test "$GIT_CONFIG_GLOBAL" = ` + shellTestQuote(os.DevNull) + `
test -z "${GIT_LFS_SKIP_SMUDGE+x}"
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
test -z "${GIT_EXEC_PATH+x}"
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
        credential.https://github.com.helper=!*) helper="${argument#credential.https://github.com.helper=!}" ;;
        credential.https://github.com.useHttpPath=true) use_http_path=true ;;
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
    test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
    test "$BUILDKITE_JOB_ID" = 11111111-1111-4111-8111-111111111111
    helper=
    filter=
    for argument in "$@"; do
      case "$argument" in
        credential.https://github.com.helper=!*) helper=true ;;
        filter.lfs.process=*) filter=true ;;
      esac
    done
    test "$helper" = true
    test "$filter" = true
    printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD
    ;;
esac
printf '%s|%s\n' "$PWD" "$*" >> ` + shellTestQuote(gitLog) + `
`
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitLFS := filepath.Join(filepath.Dir(git), "git-lfs")
	lfsScript := `#!/bin/sh
set -eu
helper=
hooks=
i=0
while [ "$i" -lt "${GIT_CONFIG_COUNT:-0}" ]; do
  eval "key=\${GIT_CONFIG_KEY_$i}"
  eval "value=\${GIT_CONFIG_VALUE_$i}"
  case "$key" in
    credential.https://github.com.helper) helper="${value#!}" ;;
    core.hooksPath) hooks="$value" ;;
  esac
  i=$((i + 1))
done
test "$hooks" = /dev/null
case "$1" in
  install)
    test -z "${BUILDKITE_AGENT_ACCESS_TOKEN+x}"
    test -z "$helper"
    ;;
  fetch)
    test "$BUILDKITE_AGENT_ACCESS_TOKEN" = job-secret
    test "$BUILDKITE_JOB_ID" = 11111111-1111-4111-8111-111111111111
    test -n "$helper"
    credentials="$(printf 'protocol=https\nhost=github.com\npath=buildkite/buildkite-gha.git\n\n' | sh -c "$helper get")"
    case "$credentials" in
      *"username=token"*"password=` + repositoryToken + `"*) ;;
      *) exit 41 ;;
    esac
    ;;
esac
printf '%s|git-lfs %s|%s core.hooksPath=%s\n' "$PWD" "$*" "$helper" "$hooks" >> ` + shellTestQuote(gitLog) + `
`
	if err := os.WriteFile(gitLFS, []byte(lfsScript), 0o700); err != nil {
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
	result, err := (Runner{Git: git, GitLFS: gitLFS, RepositoryCredentials: credentials, Stdout: &logs, Stderr: &logs}).runCheckout(t.Context(), processor, workspace, job, actionintegration.CheckoutV7Commit, map[string]string{"path": "test-catalog", "lfs": "true", "filter": "blob:none"})
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
	for _, command := range []string{
		"git-lfs install --local --skip-repo",
		"fetch --no-tags --no-recurse-submodules --progress --filter=blob:none --depth=1 origin " + sha,
		"git-lfs fetch origin " + sha,
		"checkout --detach " + sha,
	} {
		if !strings.Contains(string(gitBytes), command) {
			t.Fatalf("checkout command log lacks %q: %q", command, gitBytes)
		}
	}
	if strings.Count(string(gitBytes), "git-credentials-helper") != 3 {
		t.Fatalf("credential helper was not confined to fetch, LFS fetch, and filtered checkout commands: %q", gitBytes)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(gitBytes)), "\n") {
		if !strings.HasPrefix(line, checkoutDirectory+"|") {
			t.Fatalf("Git command ran outside checkout path %q: %q", checkoutDirectory, line)
		}
		if !strings.Contains(line, "core.hooksPath=/dev/null") {
			t.Fatalf("Git command lacks hook isolation: %q", line)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(err) {
		t.Fatalf("path checkout mutated the workspace root: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, ".no-global-gitconfig")); err != nil || !bytes.Equal(got, poisonedGlobalConfig) {
		t.Fatalf("path checkout changed poisoned root Git config: %q, %v", got, err)
	}
}

func TestValidateCheckoutRefProvenance(t *testing.T) {
	eventSHA := strings.Repeat("a", 40)
	otherSHA := strings.Repeat("b", 40)
	tests := []struct {
		name      string
		sourceRef string
		value     string
		wantError bool
	}{
		{name: "literal branch", sourceRef: "test-catalog", value: "test-catalog"},
		{name: "literal commit", sourceRef: otherSHA, value: otherSHA},
		{name: "github sha", sourceRef: "${{ github.sha }}", value: eventSHA},
		{name: "need event sha", sourceRef: "${{ needs.configure.outputs.sha }}", value: eventSHA},
		{name: "need other commit", sourceRef: "${{ needs.configure.outputs.sha }}", value: otherSHA, wantError: true},
		{name: "need branch", sourceRef: "${{ needs.configure.outputs.sha }}", value: "test-catalog", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCheckoutRefProvenance(map[string]string{"Ref": test.sourceRef}, map[string]string{"ref": test.value}, eventSHA)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "exact event SHA")) {
				t.Fatalf("validateCheckoutRefProvenance() error = %v, want exact event SHA error", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateCheckoutRefProvenance() error = %v", err)
			}
		})
	}
}

func TestAgentGitCredentialHelperCommandQuotesExecutable(t *testing.T) {
	got := agentGitCredentialHelperCommand("/tmp/agent's path")
	want := `!'/tmp/agent'\''s path' git-credentials-helper`
	if got != want {
		t.Fatalf("agentGitCredentialHelperCommand() = %q, want %q", got, want)
	}
}

func TestRepositoryProviderCredentialHelperIsGitHubSpecific(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "helper.log")
	agent := filepath.Join(t.TempDir(), "buildkite-agent")
	script := `#!/bin/sh
set -eu
test "$1" = git-credentials-helper
test "$2" = get
cat > ` + shellTestQuote(marker) + `
printf 'username=token\npassword=secret\n'
`
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	args := repositoryProviderCheckoutCredentialArgs(checkoutGitBaseArgs(), agent, "github.com")
	fill := func(host, path string) error {
		cmd := exec.Command("git", append(args, "credential", "fill")...)
		cmd.Env = processEnv(map[string]string{"GIT_TERMINAL_PROMPT": "0"})
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\npath=" + path + "\n\n")
		return cmd.Run()
	}
	if err := fill("github.com", "owner/private.git"); err != nil {
		t.Fatalf("GitHub credential fill: %v", err)
	}
	request, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(request), "path=owner/private.git") {
		t.Fatalf("helper request = %q", request)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := fill("code.example.org", "owner/public.git"); err == nil {
		t.Fatal("external host unexpectedly received credentials")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("external host invoked GitHub helper: %v", err)
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
    credential.https://github.com.helper=!*) helper="${argument#credential.https://github.com.helper=!}" ;;
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
	if _, err := (Runner{Git: git, RepositoryCredentials: credentials, Stdout: &logs, Stderr: &logs}).runCheckout(t.Context(), processor, workspace, job, actionintegration.CheckoutV7Commit, nil); err == nil {
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
	trustedGitLFS := filepath.Join(trustedDir, "git-lfs")
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
      case "$argument" in credential.https://github.com.helper=!*) helper="${argument#credential.https://github.com.helper=!}" ;; esac
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
	trustedLFSScript := "#!/bin/sh\nprintf 'git-lfs %s\\n' \"$*\" >> " + shellTestQuote(trustedLog) + "\n"
	if err := os.WriteFile(trustedGitLFS, []byte(trustedLFSScript), 0o700); err != nil {
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
	lookupGitLFS := filepath.Join(lookupDir, "git-lfs")
	if err := os.Symlink(trustedGitLFS, lookupGitLFS); err != nil {
		t.Fatal(err)
	}
	poisonDir := canonicalTempDir(t)
	poisonGitMarker := filepath.Join(t.TempDir(), "poison-git-ran")
	poisonGit := filepath.Join(poisonDir, "git")
	if err := os.WriteFile(poisonGit, []byte("#!/bin/sh\ntouch "+shellTestQuote(poisonGitMarker)+"\nexit 97\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	poisonGitLFSMarker := filepath.Join(t.TempDir(), "poison-git-lfs-ran")
	poisonGitLFS := filepath.Join(poisonDir, "git-lfs")
	if err := os.WriteFile(poisonGitLFS, []byte("#!/bin/sh\ntouch "+shellTestQuote(poisonGitLFSMarker)+"\nexit 98\n"), 0o700); err != nil {
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
  rm -f "$LOOKUP_GIT_LFS"
  ln -s "$POISON_GIT_LFS" "$LOOKUP_GIT_LFS"
  rm -f "$LOOKUP_AGENT"
  ln -s "$POISON_AGENT" "$LOOKUP_AGENT"
fi
`
	if err := os.WriteFile(node, []byte(nodeScript), 0o700); err != nil {
		t.Fatal(err)
	}

	workflowDigest := sha256.Sum256(workflowSource)
	poisonID, checkoutID := "a-0000000000000001", "a-0000000000000002"
	requiresMise := false
	job := plan.Job{
		Schema: plan.Schema,
		Compiler: plan.Compiler{
			Version: "checkout-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Runtime: &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
		Workflow: plan.Workflow{
			Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "checkout",
		},
		Event: plan.Event{
			Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64), Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha,
		},
		Target:               plan.Target{StepKey: "gha-checkout", Queue: "trusted"},
		RequiredCapabilities: []string{"network", "provider-token-read"},
		Env: map[string]string{
			"LOOKUP_AGENT":   lookupAgent,
			"LOOKUP_GIT":     lookupGit,
			"LOOKUP_GIT_LFS": lookupGitLFS,
			"POISON_AGENT":   poisonAgent,
			"POISON_GIT":     poisonGit,
			"POISON_GIT_LFS": poisonGitLFS,
		},
		Steps: []plan.Step{
			{ID: "poison", Kind: "uses", Uses: "owner/repo/poison@v1", Action: &plan.ActionSelector{Lock: poisonID}},
			{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", With: map[string]string{"filter": "blob:none", "lfs": "true"}, Action: &plan.ActionSelector{Lock: checkoutID}},
		},
		Actions: []plan.ActionLock{
			{ID: poisonID, Source: "github", Repository: "owner/repo", RequestedRef: "v1", Commit: strings.Repeat("b", 40), Path: "poison", SourceDigest: remoteDigest},
			{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: remoteDigest},
		},
		RequiresMise: &requiresMise,
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, SourceDigest: remoteDigest}}
	var logs bytes.Buffer
	credentials := &AgentRepositoryCredentials{JobID: testCacheJobID, JobToken: "job-secret"}
	result, err := (Runner{Node24: node, RepositoryCredentials: credentials, Actions: materializer, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
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
	if target, err := filepath.EvalSymlinks(lookupGitLFS); err != nil || target != poisonGitLFS {
		t.Fatalf("pre-hook Git LFS replacement = %q, %v; want %q", target, err, poisonGitLFS)
	}
	if _, err := os.Stat(trustedAgentMarker); err != nil {
		t.Fatalf("trusted Agent credential helper did not run: %v", err)
	}
	for name, marker := range map[string]string{"Git selected through poisoned PATH": poisonGitMarker, "Git LFS selected through poisoned PATH": poisonGitLFSMarker, "Agent helper selected through poisoned PATH": poisonAgentMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if strings.Contains(logs.String(), repositoryToken) {
		t.Fatalf("checkout exposed repository token in logs: %q", logs.String())
	}
	gitLog, err := os.ReadFile(trustedLog)
	resolvedTrustedGitLFS, resolveErr := filepath.EvalSymlinks(trustedGitLFS)
	if err != nil || resolveErr != nil || !strings.Contains(string(gitLog), "git-lfs install --local --skip-repo") || !strings.Contains(string(gitLog), "filter.lfs.process='"+resolvedTrustedGitLFS+"' filter-process") {
		t.Fatalf("checkout did not pin Git LFS in filter command: %q, %v", gitLog, err)
	}
}

// legacyCheckoutManifests mirror the real actions/checkout manifests at the
// admitted v1.2.0 and v2.8.0 release commits: v1.2.0 declares runs.plugin
// with no runs.using and v2.8.0 declares the retired node12 runtime, so
// neither passes generic metadata admission.
var legacyCheckoutManifests = map[string]string{
	actionintegration.CheckoutV1Commit: "name: 'Checkout'\ndescription: 'Checkout a Git repository.'\nruns:\n  plugin: 'checkout'\n",
	actionintegration.CheckoutV2Commit: "name: 'Checkout'\nruns:\n  using: node12\n  main: dist/index.js\n  post: dist/index.js\n",
}

func writeLegacyCheckoutTree(t *testing.T, commit string) (string, string) {
	t.Helper()
	remote := t.TempDir()
	manifest := legacyCheckoutManifests[commit]
	if err := os.WriteFile(filepath.Join(remote, "action.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(manifest, "dist/index.js") {
		if err := os.Mkdir(filepath.Join(remote, "dist"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(remote, "dist", "index.js"), []byte("throw new Error('adapter must not execute checkout JavaScript')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	return remote, digest
}

func TestCheckoutAdapterRunsLegacyReleaseManifests(t *testing.T) {
	for name, test := range map[string]struct {
		commit    string
		fetchWant string
		fetchSkip string
	}{
		"v1.2.0 plugin manifest": {commit: actionintegration.CheckoutV1Commit, fetchWant: "--prune origin", fetchSkip: "--depth="},
		"v2.8.0 node12 manifest": {commit: actionintegration.CheckoutV2Commit, fetchWant: "--depth=1 origin"},
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			remote, remoteDigest := writeLegacyCheckoutTree(t, test.commit)

			sha := strings.Repeat("a", 40)
			gitLog := filepath.Join(t.TempDir(), "git.log")
			git := filepath.Join(t.TempDir(), "git")
			script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> ` + shellTestQuote(gitLog) + `
operation=
for argument in "$@"; do
  case "$argument" in init|checkout) operation="$argument"; break ;; esac
done
case "$operation" in
  init) mkdir -p .git ;;
  checkout) printf '%s\n' ` + shellTestQuote(sha) + ` > .git/HEAD ;;
esac
`
			if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			workflowSource := []byte("name: legacy checkout\n")
			workflowDigest := sha256.Sum256(workflowSource)
			checkoutID := "a-0000000000000001"
			requiresMise := false
			job := plan.Job{
				Schema: plan.Schema,
				Compiler: plan.Compiler{
					Version: "checkout-test", DistributionDigest: "sha256:" + strings.Repeat("2", 64),
				},
				Runtime: &plan.Runtime{DistributionDigest: "sha256:" + strings.Repeat("2", 64)},
				Workflow: plan.Workflow{
					Path: ".github/workflows/test.yml", Digest: "sha256:" + hex.EncodeToString(workflowDigest[:]), LogicalJobID: "checkout",
				},
				Event: plan.Event{
					Provider: "github", Name: "push", PayloadDigest: "sha256:" + strings.Repeat("3", 64), Repository: "buildkite/buildkite-gha", Ref: "refs/heads/main", SHA: sha,
				},
				Target:               plan.Target{StepKey: "gha-checkout", Queue: "trusted"},
				RequiredCapabilities: []string{"network"},
				Steps: []plan.Step{
					{ID: "checkout", Kind: "uses", Uses: "actions/checkout@" + test.commit, Action: &plan.ActionSelector{Lock: checkoutID}},
				},
				Actions: []plan.ActionLock{
					{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: test.commit, Commit: test.commit, SourceDigest: remoteDigest},
				},
				RequiresMise: &requiresMise,
			}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}
			var logs bytes.Buffer
			result, err := (Runner{Git: git, Actions: materializer, Stdout: &logs, Stderr: &logs}).RunJob(t.Context(), job, workspace)
			if err != nil || result.Conclusion != "success" {
				t.Fatalf("RunJob() result = %#v, error = %v, logs = %q", result, err, logs.String())
			}
			logBytes, err := os.ReadFile(gitLog)
			if err != nil {
				t.Fatal(err)
			}
			log := string(logBytes)
			if !strings.Contains(log, test.fetchWant) {
				t.Fatalf("Git log lacks %q:\n%s", test.fetchWant, log)
			}
			if test.fetchSkip != "" && strings.Contains(log, test.fetchSkip) {
				t.Fatalf("Git log contains %q:\n%s", test.fetchSkip, log)
			}
		})
	}
}

func TestContainerPreparationSkipsNativeCheckoutClassification(t *testing.T) {
	for name, commit := range map[string]string{
		"v1.2.0 plugin manifest": actionintegration.CheckoutV1Commit,
		"v2.8.0 node12 manifest": actionintegration.CheckoutV2Commit,
	} {
		t.Run(name, func(t *testing.T) {
			remote, remoteDigest := writeLegacyCheckoutTree(t, commit)
			checkoutID := "a-0000000000000001"
			job := plan.Job{
				RequiredCapabilities: []string{"network"},
				Steps: []plan.Step{
					{ID: "checkout", Kind: "uses", Uses: "actions/checkout@" + commit, Action: &plan.ActionSelector{Lock: checkoutID}},
				},
				Actions: []plan.ActionLock{
					{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: commit, Commit: commit, SourceDigest: remoteDigest},
				},
			}
			materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}
			actions := newActionLockResolver(job, t.TempDir(), materializer)
			runner := newJobRun(Runner{})
			if err := runner.verifyRemoteActionTree(t.Context(), actions, plan.ActionSelector{Lock: checkoutID}, nil); err != nil {
				t.Fatalf("verifyRemoteActionTree() error = %v", err)
			}
			mounts, err := runner.actionContainerMounts(t.Context(), actions)
			if err != nil || len(mounts) != 0 {
				t.Fatalf("actionContainerMounts() = %#v, error = %v", mounts, err)
			}
		})
	}
}

func TestRepositoryProviderCheckoutRequiresPreResolvedGit(t *testing.T) {
	job := plan.Job{
		Event:                plan.Event{Provider: "github", Repository: "buildkite/buildkite-gha", SHA: strings.Repeat("a", 40)},
		RequiredCapabilities: []string{"provider-token-read"},
	}
	credentials := &AgentRepositoryCredentials{Agent: "/usr/bin/buildkite-agent", JobID: testCacheJobID, JobToken: "job-secret"}
	if _, err := (Runner{Git: "git", RepositoryCredentials: credentials}).runCheckout(t.Context(), newCommandProcessor(io.Discard, io.Discard), t.TempDir(), job, actionintegration.CheckoutV7Commit, nil); err == nil || !strings.Contains(err.Error(), "resolved before workflow execution") {
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
	if _, err := (Runner{}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "restricted to the verified checkout adapter") {
		t.Fatalf("ordinary provider-token-read error = %v", err)
	}
}

func TestCompositeCheckoutPreservesDynamicRefProvenance(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/test.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: nested checkout provenance\n")
	const checkoutUses = "actions/checkout@v7"
	writeFixtureFile(t, workspace, ".github/actions/outer/action.yml", "runs:\n  using: composite\n  steps:\n    - uses: "+checkoutUses+"\n      with:\n        ref: ${{ needs.configure.outputs.sha }}\n")
	outerDigest, err := source.DigestTree(filepath.Join(workspace, ".github/actions/outer"))
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	writeFixtureFile(t, remote, "action.yml", "runs:\n  using: node24\n  main: dist/index.js\n")
	writeFixtureFile(t, remote, "dist/index.js", "")
	remoteDigest, err := source.DigestTree(remote)
	if err != nil {
		t.Fatal(err)
	}
	outerID, checkoutID := "a-0000000000000001", "a-0000000000000002"
	job := runtimePlan(t, workspace, workflowPath, []plan.Step{{ID: "outer", Kind: "uses", Uses: "./.github/actions/outer", Action: &plan.ActionSelector{Lock: outerID}}})
	job.Event.Repository = "buildkite/buildkite-gha"
	job.Event.Ref = "refs/heads/main"
	job.Event.SHA = strings.Repeat("a", 40)
	job.RequiredCapabilities = []string{"network"}
	job.Needs = map[string]plan.Need{"configure": {Result: "success", Outputs: map[string]string{"sha": strings.Repeat("b", 40)}}}
	job.Actions = []plan.ActionLock{
		{ID: outerID, Source: "workspace", Path: ".github/actions/outer", SourceDigest: outerDigest, Children: map[string]plan.ActionSelector{checkoutUses: {Lock: checkoutID}}},
		{ID: checkoutID, Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: remoteDigest},
	}
	materializer := &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: remote, ActionRoot: remote, SourceDigest: remoteDigest}}

	if _, err := (Runner{Actions: materializer}).RunJob(t.Context(), job, workspace); err == nil || !strings.Contains(err.Error(), "dynamic ref must resolve to the exact event SHA") {
		t.Fatalf("nested dynamic checkout ref error = %v", err)
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
		t.Fatalf("validateJobCheckoutAdapters() = %t, %v, want unknown commit rejection", found, err)
	}
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
