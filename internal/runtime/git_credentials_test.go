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
	"time"

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
		name, permission, persistence, commit string
		external, proxy                       bool
	}{
		{name: "default read", permission: "read"},
		{name: "explicit write", permission: "write", persistence: "true"},
		{name: "checkout only", permission: "read", persistence: "false"},
		{name: "no contents", permission: "none"},
		{name: "configured credentials", permission: "none", external: true},
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
			mustGit(seed, "push", remote, "HEAD:refs/heads/main")

			token := "read-token"
			if test.permission == "write" {
				token = "write-token"
			}
			if test.external {
				token = "external-token"
			}
			backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				_, password, ok := request.BasicAuth()
				if !ok || password != token {
					w.Header().Set("WWW-Authenticate", `Basic realm="Git"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if strings.Contains(request.URL.String(), "git-receive-pack") && password != "write-token" {
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
			// The snapshot sees the administrator's HOME, not the workflow's.
			t.Setenv("HOME", root)
			authentication, err := snapshotCheckoutAuthentication(t.Context(), git, temp)
			if err != nil {
				t.Fatal(err)
			}
			provider := &testWorkflowTokenProvider{token: token}
			var output bytes.Buffer
			runner := Runner{Git: git, RuntimeExecutable: executable, WorkflowToken: provider, Redactor: &testRedactor{}}
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
				runner.RepositoryCredentials = &AgentRepositoryCredentials{proxyEnvironment: transport}
				job.Event.ServerURL = "https://" + server.Certificate().DNSNames[0]
			}
			job.GitHubToken = &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"contents": test.permission}, CheckoutOnly: true}
			processor := newCommandOutputProcessor(&output, &output)
			service, err := startGitCredentialService(t.Context(), temp, runner, job, processor)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close(t.Context()) })
			inputs := map[string]string{}
			if test.persistence != "" {
				inputs["persist-credentials"] = test.persistence
			}
			commit := test.commit
			if commit == "" {
				commit = actionintegration.CheckoutV7Commit
			}
			_, err = runner.runCheckoutWithAuthentication(t.Context(), processor, workspace, job, commit, inputs, authentication, service)
			if test.permission == "none" && !test.external {
				if err == nil || provider.calls != 0 {
					t.Fatalf("checkout without contents permission: error=%v, issued=%d", err, provider.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkout: %v\n%s", err, &output)
			}
			if got := mustGit(workspace, "rev-parse", "HEAD"); got != sha {
				t.Fatalf("checked out %s, want %s", got, sha)
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
			worktree := filepath.Join(root, "linked")
			mustGit(workspace, "worktree", "add", "--detach", worktree, "HEAD")
			mustGit(worktree, "fetch", "origin", "main")
			_, pushErr := gitRun(worktree, "push", "origin", "HEAD:refs/tags/example")
			if test.permission == "write" {
				if pushErr != nil {
					t.Fatalf("authorized tag push: %v", pushErr)
				}
				if got := mustGit(remote, "rev-parse", "refs/tags/example"); got != sha {
					t.Fatalf("remote tag = %s, want %s", got, sha)
				}
			} else if pushErr == nil {
				t.Fatal("read-only credential allowed a push")
			}
			if !test.external {
				// Renewal must keep the immutable repository/workflow/permission
				// binding, rather than taking broader authority from Git input.
				service.mu.Lock()
				service.issued = time.Now().Add(-46 * time.Minute)
				service.mu.Unlock()
				mustGit(workspace, "fetch", "origin", "main")
				if provider.calls != 2 || provider.repository != "acme/widgets" || provider.workflow != "test.yml" || provider.permissions["contents"] != test.permission {
					t.Fatalf("renewal authority: %#v", provider)
				}
			} else if provider.calls != 0 {
				t.Fatal("configured helper did not take precedence")
			}
			if err := service.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			config = mustGit(workspace, "config", "--local", "--list")
			if strings.Contains(config, service.key) {
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
				for _, mode := range []string{"success", "failure", "metadata", "directory"} {
					fail := mode == "failure"
					jobWorkspace := t.TempDir()
					command := "test -z \"${BUILDKITE_AGENT_ACCESS_TOKEN:-}\"; git fetch origin main"
					if !test.external {
						command += "; git push origin HEAD:refs/tags/runtime-example"
					}
					if fail {
						command += "; exit 1"
					}
					remove := ""
					if mode == "metadata" || mode == "directory" {
						remove = mode
					}
					fullJob := runtimePlan(t, seed, ".github/workflows/test.yml", []runtimeTestStep{
						{ID: "checkout", Kind: "uses", Uses: "actions/checkout@v7", Action: &plan.ActionSelector{Lock: "a-0000000000000001"}},
						{ID: "post", Kind: "uses", Uses: "acme/action@v1", Action: &plan.ActionSelector{Lock: "a-0000000000000002"}, Env: map[string]string{"REMOVE_CHECKOUT": remove}},
						{ID: "git", Kind: "run", Shell: "sh", Command: command},
					})
					fullJob.Env = map[string]string{"GIT_CONFIG_NOSYSTEM": "1"}
					job.Event.Name = fullJob.Event.Name
					job.Event.PayloadDigest = fullJob.Event.PayloadDigest
					fullJob.Event = job.Event
					fullJob.GitHubToken = job.GitHubToken
					fullJob.RequiredCapabilities = []string{"network", "provider-token-read", "provider-token-write"}
					*fullJob.RequiresMise = false
					fullJob.Actions = []plan.ActionLock{
						{ID: "a-0000000000000001", Source: "github", Repository: "actions/checkout", RequestedRef: "v7", Commit: actionintegration.CheckoutV7Commit, SourceDigest: digest},
						{ID: "a-0000000000000002", Source: "github", Repository: "acme/action", RequestedRef: "v1", Commit: strings.Repeat("a", 40), SourceDigest: digest},
					}
					agent, err := exec.LookPath("true")
					if err != nil {
						t.Fatal(err)
					}
					jobRunner := runner
					jobRunner.RepositoryCredentials = &AgentRepositoryCredentials{Agent: agent, JobID: testCacheJobID, JobToken: "agent-secret"}
					if test.external {
						fullJob.GitHubToken = nil
						fullJob.RequiredCapabilities = []string{"network", "provider-token-read"}
						jobRunner.RepositoryCredentials, jobRunner.WorkflowToken = nil, nil
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
					if config := mustGit(jobWorkspace, "config", "--local", "--list"); strings.Contains(config, GitCredentialHelperCommand) {
						t.Fatal("job cleanup left credential helper configured")
					}
					if _, err := gitRun(jobWorkspace, "fetch", "origin", "main"); !test.external && err == nil {
						t.Fatal("job cleanup left authenticated Git access")
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
	provider := &testWorkflowTokenProvider{token: "scoped-test-token"}
	processor := newCommandOutputProcessor(&bytes.Buffer{}, &bytes.Buffer{})
	job := plan.Job{Event: plan.Event{Provider: "github", Repository: "acme/widgets"}, GitHubToken: &plan.GitHubToken{Workflow: "test.yml", Permissions: map[string]string{"contents": "read"}}}
	service, err := startGitCredentialService(t.Context(), temp, Runner{RuntimeExecutable: buildLiveContainerRuntime(t), WorkflowToken: provider, Redactor: &testRedactor{}}, job, processor)
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
		if code := RunGitCredentialHelper([]string{service.key, "get"}, strings.NewReader(input), &output, &stderr); code != 0 || output.Len() != 0 || provider.calls != 0 {
			t.Fatalf("out-of-scope credential %q: code=%d, output=%q, calls=%d", input, code, &output, provider.calls)
		}
	}
	var output, stderr bytes.Buffer
	input := "protocol=https\nhost=github.com\npath=acme/widgets.git\n\n"
	if code := RunGitCredentialHelper([]string{"wrong-key", "get"}, strings.NewReader(input), &output, &stderr); code == 0 || output.Len() != 0 || provider.calls != 0 {
		t.Fatalf("unauthenticated broker request: code=%d, output=%q, calls=%d", code, &output, provider.calls)
	}
	if code := RunGitCredentialHelper([]string{service.key, "get"}, strings.NewReader(input), &output, &stderr); code != 0 || !strings.Contains(output.String(), fmt.Sprintf("password=%s", provider.token)) || provider.calls != 1 {
		t.Fatalf("scoped credential: code=%d, output=%q, calls=%d", code, &output, provider.calls)
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
		if output, err := cmd.Output(); err != nil || !strings.Contains(string(output), "password="+provider.token) {
			t.Fatalf("non-root container helper: error=%v, output=%q", err, output)
		}
	})
}
