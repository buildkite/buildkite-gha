package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/buildkite/buildkite-gha/internal/action/metadata"
)

func TestClassifyFailurePrecedence(t *testing.T) {
	exit := exitError(t)
	stepExit := markStepProcessExit(fmt.Errorf("step %q: %w", "test", exit))
	unsupported := errUnsupportedf("shell %q is unsupported in the supported runtime subset", "pwsh")
	hard := markHardJobFailure(errors.New("owned Docker resources remain after cleanup"))
	workflowToken := markJobSetupFailure(FailureClassWorkflowToken, credentialStatusError(400, "workflow token rejected"))
	oidcToken := markJobSetupFailure(FailureClassOIDCToken, credentialStatusError(403, "OIDC token denied"))
	cacheCredential := markJobSetupFailure(FailureClassCacheCredential, credentialStatusError(422, "cache credential rejected"))
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nil", nil, FailureClassUnknown},
		{"plain", errors.New("boom"), FailureClassUnknown},
		{"step exit", stepExit, FailureClassStepProcessExit},
		{"wrapped step exit", fmt.Errorf("run-job: %w", stepExit), FailureClassStepProcessExit},
		{"tolerated step exit", &toleratedJobFailure{err: stepExit}, FailureClassStepProcessExit},
		{"unsupported", unsupported, FailureClassUnsupportedFeature},
		{"unsupported runtime", fmt.Errorf("action %q: %w", "./action", &metadata.UnsupportedRuntimeError{Runtime: "future"}), FailureClassUnsupportedFeature},
		{"workflow token", workflowToken, FailureClassWorkflowToken},
		{"OIDC token", oidcToken, FailureClassOIDCToken},
		{"cache credential", cacheCredential, FailureClassCacheCredential},
		{"setup failure outranks step exit", errors.Join(stepExit, oidcToken), FailureClassOIDCToken},
		{"integrity", hard, FailureClassIntegrity},
		{"integrity outranks step exit", errors.Join(stepExit, hard), FailureClassIntegrity},
		{"integrity outranks setup failure", errors.Join(oidcToken, hard), FailureClassIntegrity},
		{"unsupported outranks integrity", errors.Join(hard, unsupported), FailureClassUnsupportedFeature},
		{"unsupported outranks step exit", errors.Join(stepExit, unsupported), FailureClassUnsupportedFeature},
	}
	for _, tc := range cases {
		if got := ClassifyFailure(tc.err); got != tc.want {
			t.Errorf("ClassifyFailure(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestJobSetupFailureClassifiesOnlyCredentialRequestFailures(t *testing.T) {
	err := fmt.Errorf("prepare token: %w", markJobSetupFailure(FailureClassWorkflowToken, credentialStatusError(404, "not enabled")))
	if ClassifyFailure(err) != FailureClassWorkflowToken {
		t.Fatalf("ClassifyFailure() = %q, want %q", ClassifyFailure(err), FailureClassWorkflowToken)
	}
	if status, ok := AgentAPIHTTPStatus(err); !ok || status != 404 {
		t.Fatalf("AgentAPIHTTPStatus() = %d, %t, want 404, true", status, ok)
	}
	validation := markJobSetupFailure(FailureClassWorkflowToken, errors.New("GitHub workflow token requires a valid event repository"))
	if ClassifyFailure(validation) != FailureClassUnknown {
		t.Fatalf("validation classified as %q", ClassifyFailure(validation))
	}
	if status, ok := AgentAPIHTTPStatus(errors.New("unrelated")); ok || status != 0 {
		t.Fatalf("unrelated AgentAPIHTTPStatus() = %d, %t, want 0, false", status, ok)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelDeadline := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	for _, ctx := range []context.Context{cancelled, expired} {
		failure := credentialRequestFailure(ctx, fmt.Errorf("request token: %w", ctx.Err()))
		marked := markJobSetupFailure(FailureClassWorkflowToken, failure)
		if ClassifyFailure(marked) != FailureClassUnknown {
			t.Errorf("cancellation classified as %q", ClassifyFailure(marked))
		}
		if status, ok := AgentAPIHTTPStatus(marked); ok || status != 0 {
			t.Errorf("cancellation AgentAPIHTTPStatus() = %d, %t", status, ok)
		}
	}
}

func TestAgentTokenClientTimeoutsRemainSetupFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := &http.Client{Timeout: 50 * time.Millisecond}
	workflow, err := NewAgentGitHubTokens(AgentGitHubTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "token", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	oidc, err := NewAgentOIDCTokens(AgentOIDCTokenConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "token", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewAgentCacheCredentials(AgentCacheConfig{Endpoint: server.URL, JobID: testCacheJobID, JobToken: "token", Client: client})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		class   FailureClass
		request func() error
	}{
		{FailureClassWorkflowToken, func() error {
			_, err := workflow.WorkflowToken(t.Context(), "buildkite/buildkite-gha", "ci.yml", map[string]string{"contents": "read"})
			return err
		}},
		{FailureClassOIDCToken, func() error { _, err := oidc.OIDCToken(t.Context(), "audience"); return err }},
		{FailureClassCacheCredential, func() error { _, err := cache.Credentials(t.Context()); return err }},
	} {
		t.Run(string(test.class), func(t *testing.T) {
			err := test.request()
			if !errors.Is(err, context.DeadlineExceeded) || t.Context().Err() != nil {
				t.Fatalf("request = %v, context = %v, want client timeout with live context", err, t.Context().Err())
			}
			if got := ClassifyFailure(err); got != test.class {
				t.Fatalf("ClassifyFailure() = %q, want %q", got, test.class)
			}
		})
	}
}

func TestMarkStepProcessExitRequiresExitError(t *testing.T) {
	if err := markStepProcessExit(nil); err != nil {
		t.Fatalf("markStepProcessExit(nil) = %v", err)
	}
	launch := errors.New("process /bin/missing: executable file not found")
	if got := markStepProcessExit(launch); got != launch {
		t.Fatalf("markStepProcessExit(launch failure) = %v, want unchanged", got)
	}
	exit := fmt.Errorf("process /bin/sh: %w", exitError(t))
	marked := markStepProcessExit(exit)
	if ClassifyFailure(marked) != FailureClassStepProcessExit {
		t.Fatalf("markStepProcessExit(exit) classify = %q", ClassifyFailure(marked))
	}
	if again := markStepProcessExit(marked); again != marked {
		t.Fatalf("markStepProcessExit is not idempotent: %v", again)
	}
	if !errors.As(marked, new(*exec.ExitError)) {
		t.Fatalf("marked error lost the exit error chain: %v", marked)
	}
}

func TestRunJobClassifiesStepProcessExit(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/failing.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: failing\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "fail", Kind: "run", Command: "exit 7"}})
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if got := ClassifyFailure(err); got != FailureClassStepProcessExit {
		t.Fatalf("ClassifyFailure() = %q, want %q for %v", got, FailureClassStepProcessExit, err)
	}
}

func TestRunJobClassifiesUnsupportedShell(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/shell.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: shell\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "shell", Kind: "run", Shell: "cmd", Command: "echo test"}})
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if got := ClassifyFailure(err); got != FailureClassUnsupportedFeature {
		t.Fatalf("ClassifyFailure() = %q, want %q for %v", got, FailureClassUnsupportedFeature, err)
	}
}

func TestUnsupportedShellOmitsUnprovenExecutable(t *testing.T) {
	_, err := shellCommand(`Rscript --vanilla "event value"`, "true")
	blocker, detail, ok := UnsupportedFeature(err)
	if !ok || blocker != "shell" || detail != "" {
		t.Fatalf("UnsupportedFeature() = %q, %q, %t, want shell with no detail", blocker, detail, ok)
	}
}

func TestRunJobClassifiesUnsupportedActionRuntime(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := ".github/workflows/action.yml"
	writeFixtureFile(t, workspace, workflowPath, "name: action\n")
	writeFixtureFile(t, workspace, "action/action.yml", "name: future\nruns:\n  using: future\n  main: main.js\n")
	job := runtimePlan(t, workspace, workflowPath, []runtimeTestStep{{ID: "future", Kind: "uses", Uses: "./action"}})
	var logs bytes.Buffer
	result, err := (Runner{Stdout: &logs, Stderr: &logs}).runTestJob(t.Context(), job, workspace)
	if err == nil || result.Conclusion != "failure" {
		t.Fatalf("RunJob() result = %#v, error = %v", result, err)
	}
	if got := ClassifyFailure(err); got != FailureClassUnsupportedFeature {
		t.Fatalf("ClassifyFailure() = %q, want %q for %v", got, FailureClassUnsupportedFeature, err)
	}
}

func exitError(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 7").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("sh exit error = %v", err)
	}
	return exit
}
