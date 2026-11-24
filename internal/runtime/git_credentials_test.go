package runtime

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

// Exercise Git's HTTP transport and the real CLI helper, not a fake Git that
// can accidentally accept a fetch without requesting credentials.
func TestCheckoutEphemeralCredentials(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	executable := buildLiveContainerRuntime(t)
	node24 := requireNode24(t)
	for _, test := range []struct {
		name, permission, persistence, commit              string
		external, proxy, store, submodules, siblingPrivate bool
	}{
		{name: "default read", permission: "read"},
		{name: "write declaration remains read only", permission: "write", persistence: "true"},
		{name: "checkout only", permission: "read", persistence: "false"},
		{name: "no contents", permission: "none"},
		{name: "configured credentials", permission: "none", external: true},
		{name: "configured credentials with managed access", permission: "read", external: true},
		{name: "store cannot retain managed token", permission: "read", store: true},
		{name: "private sibling submodule", permission: "read", submodules: true},
		{name: "private sibling later fetch", permission: "read", siblingPrivate: true},
		{name: "proxy transport", permission: "read", proxy: true},
		{name: "legacy v1.0.0", permission: "read", commit: "af513c7a016048ae468971c52ed77d9562c7c819"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, workspace := t.TempDir(), t.TempDir()
			temp, err := os.MkdirTemp("", "gha-") //nolint:usetesting // t.TempDir paths exceed macOS Unix socket limits.
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(temp) })
			transport := map[string]string{}
			gitRun := func(directory string, args ...string) (string, error) {
				cmd := exec.CommandContext(t.Context(), git, args...)
				cmd.Dir = directory
				cmd.Env = processEnv(mergeStringMaps(transport, map[string]string{"HOME": root, "RUNNER_TEMP": temp, "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1"}))
				output, err := cmd.CombinedOutput()
				return string(output), err
			}
			mustGit := func(directory string, args ...string) string {
				t.Helper()
				output, err := gitRun(directory, args...)
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(output)
			}
			remote := filepath.Join(root, "acme", "widgets.git")
			mustGit(root, "init", "--bare", remote)
			mustGit(remote, "config", "http.receivepack", "true")
			seed := filepath.Join(root, "seed")
			mustGit(root, "init", seed)
			writeFixtureFile(t, seed, ".github/workflows/test.yml", "name: checkout credentials\n")
			mustGit(seed, "add", ".")
			mustGit(seed, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "-m", "initial")
			sha := mustGit(seed, "rev-parse", "HEAD")
			if test.submodules || test.siblingPrivate {
				sibling := filepath.Join(root, "acme", "library.git")
				mustGit(root, "init", "--bare", sibling)
				mustGit(seed, "push", sibling, "HEAD:refs/heads/main")
			}
			if test.submodules {
				writeFixtureFile(t, seed, ".gitmodules", "[submodule \"library\"]\npath = library\nurl = ../library.git\n")
				mustGit(seed, "add", ".gitmodules")
				mustGit(seed, "update-index", "--add", "--cacheinfo", "160000,"+sha+",library")
				mustGit(seed, "-c", "user.name=Example", "-c", "user.email=example@example.com", "commit", "-m", "add submodule")
				sha = mustGit(seed, "rev-parse", "HEAD")
			}
			mustGit(seed, "push", remote, "HEAD:refs/heads/main")

			token := "read-token"
			if test.external {
				token = "external-token"
			}
			backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				_, password, ok := request.BasicAuth()
				wantToken := token
				if test.siblingPrivate && strings.HasPrefix(request.URL.Path, "/acme/library.git/") {
					wantToken = "external-token"
				}
				if !ok || password != wantToken {
					w.Header().Set("WWW-Authenticate", `Basic realm="Git"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if strings.Contains(request.URL.String(), "git-receive-pack") && password != "external-token" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				backend.ServeHTTP(w, request)
			}))
			defer server.Close()
			certificate := filepath.Join(root, "ca.pem")
			if err := os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			mustGit(root, "config", "--global", "http.sslCAInfo", certificate)
			if test.external {
				mustGit(root, "config", "--global", "credential.helper", "!f() { printf 'username=external\\npassword=external-token\\n'; }; f")
			}
			if test.siblingPrivate {
				mustGit(root, "config", "--global", "credential.useHttpPath", "true")
				mustGit(root, "config", "--global", "credential.helper", "!f() { request=$(cat); case \"$request\" in *path=acme/library.git*) printf 'username=external\\npassword=external-token\\n' ;; esac; }; f")
			}
			store := filepath.Join(root, "stored-credentials")
			if test.store {
				mustGit(root, "config", "--global", "credential.helper", "store --file="+store)
			}
			agent := filepath.Join(root, "buildkite-agent")
			marker := filepath.Join(root, "agent-lookups")
			writeFixtureFile(t, root, "buildkite-agent", "#!/bin/sh\nset -eu\ntest \"$BUILDKITE_AGENT_ACCESS_TOKEN\" = agent-secret\ntest \"$1 $2\" = 'git-credentials-helper get'\ncat >/dev/null\necho lookup >> "+shellTestQuote(marker)+"\nprintf 'username=provider-user\\npassword=read-token\\n\\n'\n")
			if err := os.Chmod(agent, 0o700); err != nil {
				t.Fatal(err)
			}
			// The snapshot sees the administrator's HOME, not the workflow's.
			t.Setenv("HOME", root)
			authentication, err := snapshotCheckoutAuthentication(t.Context(), git, temp)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			runner := Runner{Git: git, RuntimeExecutable: executable, RepositoryCredentials: &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "agent-secret"}}
			job := plan.Job{Event: plan.Event{Provider: "github", Repository: "acme/widgets", SHA: sha, Ref: "refs/heads/main", ServerURL: server.URL}}
			if test.proxy {
				// Only the proxy can route the certificate's DNS name to this
				// private test server. Dropping HTTPS_PROXY must break checkout.
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					upstream, err := net.Dial("tcp", server.Listener.Addr().String())
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadGateway)
						return
					}
					defer func() { _ = upstream.Close() }()
					client, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						return
					}
					defer func() { _ = client.Close() }()
					_, _ = fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
					go func() { _, _ = io.Copy(upstream, client) }()
					_, _ = io.Copy(client, upstream)
				}))
				defer proxy.Close()
				transport["HTTPS_PROXY"], transport["NO_PROXY"] = proxy.URL, ""
				runner.RepositoryCredentials.proxyEnvironment = transport
				job.Event.ServerURL = "https://" + server.Certificate().DNSNames[0]
			}
			processor := newCommandOutputProcessor(&output, &output)
			var service *gitCredentialService
			if test.permission != "none" {
				job.RequiredCapabilities = []string{"network", "provider-token-read"}
				service, err = startGitCredentialService(t.Context(), temp, runner, job, processor)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = service.Close(t.Context()) })
			}
			inputs := map[string]string{}
			if test.persistence != "" {
				inputs["persist-credentials"] = test.persistence
			}
			if test.submodules {
				inputs["submodules"] = "true"
			}
			commit := test.commit
			if commit == "" {
				commit = actionintegration.CheckoutV7Commit
			}
			_, err = runner.runCheckoutWithAuthentication(t.Context(), processor, workspace, job, commit, inputs, authentication, service)
			if test.permission == "none" && !test.external {
				if _, markerErr := os.Stat(marker); err == nil || !os.IsNotExist(markerErr) {
					t.Fatalf("checkout without contents permission: error=%v, helper marker=%v", err, markerErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkout: %v\n%s", err, &output)
			}
			if got := mustGit(workspace, "rev-parse", "HEAD"); got != sha {
				t.Fatalf("checked out %s, want %s", got, sha)
			}
			if test.submodules {
				if _, err := os.Stat(filepath.Join(workspace, "library", ".github", "workflows", "test.yml")); err != nil {
					t.Fatalf("private sibling submodule was not checked out: %v", err)
				}
			}
			config := mustGit(workspace, "config", "--local", "--list")
			if strings.Contains(config, token) || strings.Contains(output.String(), token) {
				t.Fatal("checkout wrote the bearer token to Git config or logs")
			}
			_, fetchErr := gitRun(workspace, "fetch", "origin", "main")
			if test.persistence == "false" || test.commit != "" {
				if fetchErr == nil {
					t.Fatal("persist-credentials:false authenticated a later fetch")
				}
				return
			}
			if fetchErr != nil {
				t.Fatalf("later fetch: %v", fetchErr)
			}
			if test.siblingPrivate {
				mustGit(workspace, "fetch", server.URL+"/acme/library.git", "main")
			}
			worktree := filepath.Join(root, "linked")
			mustGit(workspace, "worktree", "add", "--detach", worktree, "HEAD")
			mustGit(worktree, "fetch", "origin", "main")
			_, pushErr := gitRun(worktree, "push", "origin", "HEAD:refs/tags/example")
			if test.external {
				if pushErr != nil {
					t.Fatalf("authorized tag push: %v", pushErr)
				}
				if got := mustGit(remote, "rev-parse", "refs/tags/example"); got != sha {
					t.Fatalf("remote tag = %s, want %s", got, sha)
				}
			} else if pushErr == nil {
				t.Fatal("read-only credential allowed a push")
			}
			if test.external {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("configured helper did not take precedence")
				}
			}
			if test.store {
				if _, err := os.Stat(store); !os.IsNotExist(err) {
					t.Fatal("Git persisted a managed credential in the inherited store")
				}
			}
			if service != nil {
				if err := service.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			config = mustGit(workspace, "config", "--local", "--list")
			if service != nil && strings.Contains(config, service.key) {
				t.Fatal("job cleanup left the temporary credential helper configured")
			}
			if !test.external {
				if output, err := gitRun(workspace, "fetch", "origin", "main"); err == nil {
					t.Fatalf("fetch authenticated after job cleanup: %s", output)
				}
			}
			if test.permission == "write" || test.external {
				// Run the public job interface as well: a helper attached by the
				// adapter must remain usable in shell and post steps, then vanish.
				actionRoot := t.TempDir()
				writeFixtureFile(t, actionRoot, "action.yml", "runs:\n  using: node24\n  main: main.js\n  post: post.js\n")
				writeFixtureFile(t, actionRoot, "main.js", "")
				writeFixtureFile(t, actionRoot, "post.js", "require('child_process').execFileSync('git', ['fetch', 'origin', 'main'], {stdio: 'inherit'}); if (process.env.REMOVE_CHECKOUT) require('fs').rmSync(process.env.REMOVE_CHECKOUT === 'metadata' ? '.git' : process.cwd(), {recursive: true, force: true}); console.log('post-fetch-ok');\n")
				digest := digestTree(t, actionRoot)
				for _, mode := range []string{"success", "failure", "metadata", "directory", "parallel"} {
					fail := mode == "failure"
					jobWorkspace := t.TempDir()
					command := "test -z \"${BUILDKITE_AGENT_ACCESS_TOKEN:-}\"; git fetch origin main"
					if fail {
						command += "; exit 1"
					}
					remove := ""
					if mode == "metadata" || mode == "directory" {
						remove = mode
					}
					postEnv := map[string]string{"REMOVE_CHECKOUT": remove}
					steps := []runtimeTestStep{
						{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: "a-0000000000000001"}},
						{ID: "post", Kind: "uses", Uses: "acme/action@v1", Action: &plan.ActionSelector{Lock: "a-0000000000000002"}, Env: postEnv},
						{ID: "git", Kind: "run", Shell: "sh", Command: command},
					}
					checkouts := []string{jobWorkspace}
					if mode == "parallel" {
						steps[0].With, steps[0].Background = map[string]string{"path": "first"}, true
						steps[2].Command = "git -C first fetch origin main && git -C second fetch origin main"
						steps = append([]runtimeTestStep{
							steps[0],
							{ID: "second", Kind: "uses", Uses: "actions/checkout@v7", With: map[string]string{"path": "second"}, Background: true, Action: &plan.ActionSelector{Lock: "a-0000000000000001"}},
							{ID: "join", Kind: "wait", Targets: []string{"checkout", "second"}},
						}, steps[1:]...)
						checkouts = []string{filepath.Join(jobWorkspace, "first"), filepath.Join(jobWorkspace, "second")}
						postEnv["GIT_DIR"], postEnv["GIT_WORK_TREE"] = filepath.Join(checkouts[0], ".git"), checkouts[0]
					}
					fullJob := runtimePlan(t, seed, ".github/workflows/test.yml", steps)
					fullJob.Env = map[string]string{"GIT_CONFIG_NOSYSTEM": "1"}
					job.Event.Name = fullJob.Event.Name
					job.Event.PayloadDigest = fullJob.Event.PayloadDigest
					fullJob.Event = job.Event
					fullJob.RequiredCapabilities = []string{"network", "provider-token-read"}
					*fullJob.RequiresMise = false
					fullJob.Actions = []plan.ActionLock{
						{ID: "a-0000000000000001", Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: digest},
						{ID: "a-0000000000000002", Source: "github", Repository: "acme/action", RequestedRef: "v1", Commit: strings.Repeat("a", 40), SourceDigest: digest},
					}
					jobRunner := runner
					jobRunner.RepositoryCredentials = &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "agent-secret"}
					if test.external {
						fullJob.GitHubToken = nil
						fullJob.RequiredCapabilities = []string{"network", "provider-token-read"}
						jobRunner.RepositoryCredentials = nil
					}
					jobRunner.Node24 = node24
					jobRunner.Actions = &fakeActionMaterializer{result: source.Materialized{RepositoryRoot: actionRoot, ActionRoot: actionRoot, SourceDigest: digest}}
					var logs bytes.Buffer
					jobRunner.Stdout, jobRunner.Stderr = &logs, &logs
					result, err := jobRunner.runTestJob(t.Context(), fullJob, jobWorkspace)
					want := "success"
					if fail {
						want = "failure"
					}
					if result.Conclusion != want || (!fail && err != nil) || !strings.Contains(logs.String(), "post-fetch-ok") {
						t.Fatalf("job lifecycle: result=%s, error=%v\n%s", result.Conclusion, err, &logs)
					}
					if mode == "metadata" || mode == "directory" {
						if _, err := os.Stat(filepath.Join(jobWorkspace, ".git")); !os.IsNotExist(err) {
							t.Fatalf("post action did not remove Git metadata: %v", err)
						}
						continue
					}
					for _, directory := range checkouts {
						if config := mustGit(directory, "config", "--local", "--list"); strings.Contains(config, GitCredentialHelperCommand) {
							t.Fatal("job cleanup left credential helper configured")
						}
						if _, err := gitRun(directory, "fetch", "origin", "main"); !test.external && err == nil {
							t.Fatal("job cleanup left authenticated Git access")
						}
					}
				}
			}
		})
	}
}

func TestGitCredentialHelperRepositoryBoundary(t *testing.T) {
	temp, err := os.MkdirTemp("", "gha-") //nolint:usetesting // t.TempDir paths exceed macOS Unix socket limits.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	const token = "scoped-test-token"
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(temp, "agent")
	marker := filepath.Join(temp, "lookups")
	response := filepath.Join(temp, "response")
	writeFixtureFile(t, temp, "response", "username=provider-user\npassword="+token+"\n\n")
	writeFixtureFile(t, temp, "agent", "#!/bin/sh\nset -eu\ntest \"$BUILDKITE_AGENT_ACCESS_TOKEN\" = agent-secret\ncat >/dev/null\necho lookup >> "+shellTestQuote(marker)+"\ncat "+shellTestQuote(response)+"\n")
	if err := os.Chmod(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	processor := newCommandOutputProcessor(&bytes.Buffer{}, &bytes.Buffer{})
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "acme/widgets"}}
	service, err := startGitCredentialService(t.Context(), temp, Runner{Git: git, RuntimeExecutable: buildLiveContainerRuntime(t), RepositoryCredentials: &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "agent-secret"}}, job, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close(t.Context()) }()
	t.Setenv("RUNNER_TEMP", temp)
	for _, input := range []string{
		"protocol=http\nhost=github.com\npath=acme/widgets.git\n\n",
		"protocol=https\nhost=elsewhere.example\npath=acme/widgets.git\n\n",
		"protocol=https\nhost=github.com\npath=acme/other.git\n\n",
		"protocol=https\nhost=github.com\npath=acme/widgets.git/extra\n\n",
	} {
		var output, stderr bytes.Buffer
		code := RunGitCredentialHelper([]string{service.key, "get"}, strings.NewReader(input), &output, &stderr)
		if _, err := os.Stat(marker); code != 0 || output.Len() != 0 || !os.IsNotExist(err) {
			t.Fatalf("out-of-scope credential %q: code=%d, output=%q, lookup=%v", input, code, &output, err)
		}
	}
	var output, stderr bytes.Buffer
	input := "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n"
	if code := RunGitCredentialHelper([]string{"wrong-key", "get"}, strings.NewReader(input), &output, &stderr); code == 0 || output.Len() != 0 {
		t.Fatalf("unauthenticated broker request: code=%d, output=%q", code, &output)
	}
	if code := RunGitCredentialHelper([]string{service.key, "get"}, strings.NewReader(input), &output, &stderr); code != 0 || !strings.Contains(output.String(), "username=provider-user\npassword="+token) {
		t.Fatalf("scoped credential: code=%d, output=%q, stderr=%s", code, &output, &stderr)
	}
	writeFixtureFile(t, temp, "response", "username=provider-user\npassword=rotated-"+token+"\n\n")
	output.Reset()
	if code := RunGitCredentialHelper([]string{service.key, "get"}, strings.NewReader(input), &output, &stderr); code != 0 || !strings.Contains(output.String(), "password=rotated-"+token) {
		t.Fatalf("provider credential rotation was hidden by a local cache: code=%d, stderr=%s", code, &stderr)
	}
	t.Run("non-root container", func(t *testing.T) {
		requireLinuxAMD64(t)
		docker := requireDocker(t)
		if err := os.Chmod(temp, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), docker, "run", "--rm", "-i", "--user", "65534:65534", "-e", "RUNNER_TEMP=/credentials", "-v", temp+":/credentials", "alpine:3.20", "/credentials/"+gitCredentialExecutable(), GitCredentialHelperCommand, service.key, "get")
		cmd.Stdin = strings.NewReader(input)
		cmd.Stderr = io.Discard
		if output, err := cmd.Output(); err != nil || !strings.Contains(string(output), "password=rotated-"+token) {
			t.Fatalf("non-root container helper: error=%v, output=%q", err, output)
		}
	})
}
