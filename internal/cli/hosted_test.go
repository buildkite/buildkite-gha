package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	actionsource "github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/compatibility"
	"github.com/buildkite/buildkite-gha/internal/compiler"
	"github.com/buildkite/buildkite-gha/internal/plan"
	"go.yaml.in/yaml/v4"
)

func TestConfiguredLinuxRunnerTargetsDefaultHostedToolchainImages(t *testing.T) {
	for _, test := range []struct {
		label string
		image string
	}{
		{label: "Ubuntu-Latest", image: defaultNobleRunnerImage},
		{label: "ubuntu-24.04", image: defaultNobleRunnerImage},
		{label: "ubuntu-22.04", image: defaultJammyRunnerImage},
	} {
		t.Run(test.label, func(t *testing.T) {
			canonical, target, err := configuredRunnerTarget(test.label, "hosted", "")
			if err != nil {
				t.Fatal(err)
			}
			if canonical != strings.ToLower(test.label) || target != (compiler.RunnerTarget{Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: test.image}) {
				t.Fatalf("configuredRunnerTarget() = %q, %#v", canonical, target)
			}
		})
	}

	override := "registry.example.com/custom@sha256:" + strings.Repeat("0", 64)
	_, target, err := configuredRunnerTarget("ubuntu-latest", "hosted", override)
	if err != nil {
		t.Fatal(err)
	}
	if target.Image != override {
		t.Fatalf("explicit image = %q, want %q", target.Image, override)
	}

	canonical, target, err := configuredRunnerTarget("ubuntu-18.04", "legacy-linux", "")
	if err != nil || canonical != "ubuntu-18.04" || target != (compiler.RunnerTarget{Queue: "legacy-linux", Platform: compiler.PlatformLinuxAMD64}) {
		t.Fatalf("fallback override = %q, %#v, %v", canonical, target, err)
	}
}

func TestHostedRunnerTargetsContainOnlyHostedGuarantees(t *testing.T) {
	targets := hostedRunnerTargets()
	labels := make([]string, 0, len(targets))
	for label := range targets {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	want := []string{"macos-latest", "ubuntu-22.04", "ubuntu-24.04", "ubuntu-latest"}
	if !slices.Equal(labels, want) {
		t.Fatalf("hosted runner labels = %q, want %q", labels, want)
	}
	if got := targets["macos-latest"]; got != (compiler.RunnerTarget{Queue: defaultMacOSRunnerQueue, Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("macos-latest target = %#v", got)
	}
	for _, label := range []string{"macos-14", "macos-15", "ubuntu-24.04-arm", "windows-latest"} {
		if _, ok := targets[label]; ok {
			t.Errorf("hosted runner preset unexpectedly contains %q", label)
		}
	}

	canonical, target, err := configuredRunnerTarget("macOS-15", "organization-macos", "")
	if err != nil || canonical != "macos-15" || target != (compiler.RunnerTarget{Queue: "organization-macos", Platform: compiler.PlatformDarwinARM64}) {
		t.Fatalf("organization macOS target = %q, %#v, %v", canonical, target, err)
	}
}

func TestHostedPreflightCompilesPublicReusableWorkflowWithSharedSource(t *testing.T) {
	callerRoot := t.TempDir()
	callerPath := filepath.Join(callerRoot, ".github", "workflows", "caller.yml")
	if err := os.MkdirAll(filepath.Dir(callerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	caller := []byte("on: push\njobs:\n  delegated:\n    uses: owner/repository/.github/workflows/ci.yml@v1\n")
	if err := os.WriteFile(callerPath, caller, 0o600); err != nil {
		t.Fatal(err)
	}
	remoteRoot := t.TempDir()
	remotePath := filepath.Join(remoteRoot, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(remotePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remotePath, []byte("on: workflow_call\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := actionsource.DigestTree(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	source := &batchCountingActionSource{root: remoteRoot, digest: digest}
	shared := compiler.MemoizeRepositorySource(source)
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	distributionDigest := "sha256:" + strings.Repeat("d", 64)
	compiled, err := compileHostedWithActionCache(t.Context(), callerPath, caller, event, "0.0.0-test", distributionDigest, "importer", "", nil, map[compiler.Platform]string{compiler.PlatformLinuxAMD64: distributionDigest}, "", shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.HasActions || len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Workflow.Remote == nil || compiled.Bundle.Plans[0].Job.Workflow.Remote.Commit != strings.Repeat("a", 40) {
		t.Fatalf("hosted remote workflow result = %#v", compiled)
	}
	if source.calls != 1 {
		t.Fatalf("repository source calls = %d, want one across hosted preflight and plans", source.calls)
	}
}

func TestHostedReportRetainsIndependentActionInvocationResultsThroughWrapper(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "actions.yml")
	actionPath := filepath.Join(root, ".github", "actions", "dynamic")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  duplicate:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/dynamic
        with:
          value: supplied
      - uses: ./.github/actions/dynamic
  missing:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/missing-one
      - uses: ./.github/actions/missing-two
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	action := []byte(`name: dynamic
inputs:
  value:
    default: ${{ github[env.NAME] }}
runs:
  using: node24
  main: main.js
`)
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), action, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "main.js"), []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
	if code := Run(args, &stdout, &stderr, "dev"); code != 1 {
		t.Fatalf("Run() code = %d, want 1; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 3 {
		t.Fatalf("diagnostics = %#v, want three independent action failures", report.Diagnostics)
	}
	wantResults := map[int]string{1: compatibility.Passed, 2: compatibility.Failed}
	missingFailures := 0
	for _, result := range report.Actions {
		switch result.Job {
		case "gha-duplicate":
			if result.Result != wantResults[result.Step] {
				t.Fatalf("duplicate action result = %#v", result)
			}
		case "gha-missing":
			if result.Result == compatibility.Failed {
				missingFailures++
			}
		}
	}
	if missingFailures != 2 {
		t.Fatalf("actions = %#v, want both missing invocations failed", report.Actions)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Instance == "" || diagnostic.Step == 0 || diagnostic.Code != compiler.CodeActionResolution || !strings.Contains(diagnostic.Message, `Action "`) || !strings.Contains(diagnostic.Message, "unsupported") {
			t.Fatalf("diagnostic lacks invocation identity: %#v", diagnostic)
		}
	}
}

func TestHostedReportAdmitsIndependentContainerJobs(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "containers.yml")
	workflow := []byte(`on: push
jobs:
  first:
    runs-on: ubuntu-latest
    container: alpine:3.20
    steps:
      - run: true
  second:
    runs-on: ubuntu-latest
    container: alpine:3.20
    steps:
      - run: true
`)
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	args := []string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}
	if code := Run(args, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	instances := map[string]string{}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == "E_PROFILE" {
			t.Fatalf("unexpected profile diagnostic: %#v", diagnostic)
		}
	}
	for _, job := range report.Jobs {
		if job.Instance != "" {
			instances[job.Instance] = job.Result
		}
	}
	if report.Result != "admitted" || report.Admission.Result != "admitted" || instances["gha-first"] != compatibility.Passed || instances["gha-second"] != compatibility.Passed {
		t.Fatalf("report = %#v", report)
	}
}

func TestHostedProfileReportsNarrowedReusableWorkflowTokenWarning(t *testing.T) {
	root := t.TempDir()
	workflowRoot := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflowRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(workflowRoot, "caller.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\npermissions:\n  contents: write\njobs:\n  delegated:\n    uses: ./.github/workflows/reusable.yml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowRoot, "reusable.yml"), []byte("on: workflow_call\npermissions:\n  contents: read\njobs:\n  token:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ secrets.GITHUB_TOKEN }}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "W_REUSABLE_WORKFLOW_TOKEN_USES_ROOT_PERMISSIONS" || report.Diagnostics[0].Level != "warning" {
		t.Fatalf("diagnostics = %#v, want reusable workflow token warning", report.Diagnostics)
	}
}

func TestValidateHostedProfileResolvesActionsWithoutClaimingRuntime(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "action.yml")
	actionPath := filepath.Join(root, ".github", "actions", "local")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: node24\n  main: main.js\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "main.js"), []byte("console.log('local action')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDKITE_GHA_NODE20", writeFakeNode(t, root, 20))
	t.Setenv("BUILDKITE_GHA_NODE24", writeFakeNode(t, root, 24))
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "W_ACTION_RUNTIME_UNKNOWN" {
		t.Fatalf("profile report = %#v", report)
	}

	t.Setenv("BUILDKITE_GHA_NODE20", filepath.Join(root, "missing-node20"))
	t.Setenv("BUILDKITE_GHA_TARGET_QUEUE", "not a queue")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("environment override affected production profile: code = %d; stderr = %q", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" {
		t.Fatalf("environment profile report = %#v", report)
	}
}

func TestValidateHostedProfileAdmitsOrdinarySecretsAfterCompile(t *testing.T) {
	workflowPath := filepath.Join(t.TempDir(), "secret.yml")
	if err := os.WriteFile(workflowPath, []byte("on: push\njobs:\n  secret:\n    runs-on: ubuntu-latest\n    env:\n      TOKEN: ${{ secrets.TOKEN }}\n    steps:\n      - run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted-tokenless", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || report.Compile.Result != "compilable" || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
		t.Fatalf("profile report = %#v", report)
	}
}

func TestValidateHostedProfileAdmitsImplicitReadOnlyWorkflowToken(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "token.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  token:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ secrets.GITHUB_TOKEN }}'\n")
	if err := os.WriteFile(workflowPath, workflow, 0o600); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join("..", "..", "testdata", "smoke", "events", "push.json")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"validate", "--profile", "hosted", "--format", "json", "--event-path", eventPath, workflowPath}, &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	var report compatibility.ProcessingReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Result != "admitted" || report.Compile.Result != "compilable" || report.Admission.Result != "admitted" || len(report.Diagnostics) != 0 {
		t.Fatalf("profile report = %#v", report)
	}
}

func TestActionSourceAuthenticationUsesDedicatedTokenAndFallsBackAnonymously(t *testing.T) {
	const token = "ghs_action_source"
	provider := &cliActionSourceTokenProvider{token: token}
	redactor := &cliRedactor{}
	authentication := &actionSourceAuthentication{provider: provider, redactor: redactor}
	option := authentication.option("buildkite/buildkite-gha")
	if option == nil || provider.calls != 0 || len(redactor.values) != 0 {
		t.Fatalf("authentication = option %v, provider %#v, redactions %#v", option != nil, provider, redactor.values)
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/repos/o/r" {
			_, _ = io.WriteString(w, `{"private":false,"visibility":"public"}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}}`)
	}))
	defer server.Close()
	resolver, err := actionsource.NewResolver(server.Client(), option, actionsource.WithTestEndpoints(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	exact, err := actionsource.Parse("o/r@0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), exact); err != nil || requests != 0 || provider.calls != 0 || len(redactor.values) != 0 {
		t.Fatalf("exact Resolve() error/requests/provider/redactions = %v / %d / %d / %#v", err, requests, provider.calls, redactor.values)
	}
	ref, err := actionsource.Parse("o/r@v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), ref); err != nil || requests != 2 || provider.calls != 1 || provider.repository != "buildkite/buildkite-gha" || !slices.Equal(redactor.values, []string{token}) {
		t.Fatalf("Resolve() error/requests = %v / %d", err, requests)
	}
	second, _ := actionsource.Parse("o/r@v2")
	if _, err := resolver.Resolve(t.Context(), second); err != nil || provider.calls != 1 || !slices.Equal(redactor.values, []string{token}) {
		t.Fatalf("second Resolve() error/provider/redactions = %v / %d / %#v", err, provider.calls, redactor.values)
	}

	for _, test := range []struct {
		name          string
		provider      *cliActionSourceTokenProvider
		redactor      *cliRedactor
		cancelContext bool
		wantContext   bool
	}{
		{name: "unavailable", redactor: &cliRedactor{}},
		{name: "mint failure", provider: &cliActionSourceTokenProvider{err: errors.New("secret backend details")}, redactor: &cliRedactor{}},
		{name: "redaction failure", provider: &cliActionSourceTokenProvider{token: token}, redactor: &cliRedactor{err: errors.New("secret backend details")}},
		{name: "mint client timeout", provider: &cliActionSourceTokenProvider{err: context.DeadlineExceeded}, redactor: &cliRedactor{}},
		{name: "redaction client timeout", provider: &cliActionSourceTokenProvider{token: token}, redactor: &cliRedactor{err: context.DeadlineExceeded}},
		{name: "pre-cancelled while unavailable", redactor: &cliRedactor{}, cancelContext: true, wantContext: true},
		{name: "mint cancellation", provider: &cliActionSourceTokenProvider{err: context.Canceled}, redactor: &cliRedactor{}, wantContext: true},
		{name: "redaction cancellation", provider: &cliActionSourceTokenProvider{token: token}, redactor: &cliRedactor{err: context.Canceled}, wantContext: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var warnings bytes.Buffer
			ctx := t.Context()
			if test.cancelContext {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			authentication := &actionSourceAuthentication{redactor: test.redactor, warnings: &warnings}
			if test.provider != nil {
				authentication.provider = test.provider
			}
			gotToken, err := authentication.token(ctx, "buildkite/buildkite-gha")
			switch {
			case test.wantContext:
				if !errors.Is(err, context.Canceled) || gotToken != "" || warnings.Len() != 0 {
					t.Fatalf("token/error/warnings = %q / %v / %q", gotToken, err, warnings.String())
				}
			case err != nil || gotToken != "":
				t.Fatalf("token/error = %q / %v, want anonymous fallback", gotToken, err)
			case !strings.Contains(warnings.String(), "resolving mutable public action references anonymously") || strings.Contains(warnings.String(), token) || strings.Contains(warnings.String(), "backend details"):
				t.Fatalf("fallback warning = %q, want sanitized observable warning", warnings.String())
			}
		})
	}
}

func TestActionSourceAuthenticationReusesOneTokenAcrossConcurrentWorkflowResolvers(t *testing.T) {
	const token = "ghs_action_source"
	provider := &cliActionSourceTokenProvider{token: token}
	redactor := &cliRedactor{}
	authentication := &actionSourceAuthentication{provider: provider, redactor: redactor}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			_, _ = io.WriteString(w, `{"private":false,"visibility":"public"}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}}`)
	}))
	defer server.Close()

	const resolverCount = 11
	resolvers := make([]*actionsource.Resolver, resolverCount)
	for i := range resolvers {
		resolver, err := actionsource.NewResolver(server.Client(), authentication.option("buildkite/buildkite-gha"), actionsource.WithTestEndpoints(server.URL))
		if err != nil {
			t.Fatal(err)
		}
		resolvers[i] = resolver
	}
	ref, err := actionsource.Parse("o/r@v1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, resolverCount)
	for _, resolver := range resolvers {
		wg.Go(func() {
			_, err := resolver.Resolve(t.Context(), ref)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if provider.calls != 1 || !slices.Equal(redactor.values, []string{token}) {
		t.Fatalf("provider calls/redactions = %d / %#v, want one importer-job mint and redaction", provider.calls, redactor.values)
	}
}

func TestHostedLocalActionDoesNotProvisionSourceToken(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, ".github", "workflows", "local.yml")
	actionPath := filepath.Join(root, ".github", "actions", "local")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(actionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowSource := []byte("on: push\njobs:\n  local:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: ./.github/actions/local\n")
	if err := os.WriteFile(workflowPath, workflowSource, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionPath, "action.yml"), []byte("runs:\n  using: composite\n  steps:\n    - shell: sh\n      run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eventSource, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	originEventSource := bytes.Replace(eventSource, []byte(`"provider": "github"`), []byte(`"provider": "cursor-origin"`), 1)
	originEventSource = bytes.Replace(originEventSource, []byte(`"owner": "buildkite"`), []byte(`"owner": "acme_team"`), 1)
	originEventSource = bytes.Replace(originEventSource, []byte(`"name": "buildkite-gha"`), []byte(`"name": "widgets"`), 1)
	originEventSource = bytes.Replace(originEventSource, []byte(`"clone_url": "https://github.com/buildkite/buildkite-gha.git"`), []byte(`"clone_url": "https://origin.cursor.com/git/acme_team/widgets.git"`), 1)
	for _, test := range []struct {
		name  string
		event []byte
	}{
		{name: "GitHub", event: eventSource},
		{name: "Origin repository with GitHub-incompatible namespace", event: originEventSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &cliActionSourceTokenProvider{token: "must-not-be-minted"}
			redactor := &cliRedactor{}
			var warnings bytes.Buffer
			authentication := &actionSourceAuthentication{provider: provider, redactor: redactor, warnings: &warnings}
			if _, err := compileHosted(t.Context(), workflowPath, workflowSource, test.event, "dev", "sha256:"+strings.Repeat("0", 64), "importer", "", nil, nil, authentication); err != nil {
				t.Fatal(err)
			}
			if provider.calls != 0 || len(redactor.values) != 0 || warnings.Len() != 0 {
				t.Fatalf("local action provisioned source credential: calls %d, redactions %#v, warnings %q", provider.calls, redactor.values, warnings.String())
			}
		})
	}
}

func TestCompileHostedRequiresExplicitMacOSQueueAndRuntime(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  macos:\n    runs-on: macos-15\n    steps:\n      - run: echo macos\n")
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	compilerDigest := "sha256:" + strings.Repeat("0", 64)
	darwinDigest := "sha256:" + strings.Repeat("1", 64)
	_, err = compileHosted(t.Context(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `runner label "macos-15" is not mapped by policy`) {
		t.Fatalf("missing macOS queue error = %v", err)
	}
	macOSTarget := map[string]compiler.RunnerTarget{"macos-15": {Queue: "macos", Platform: compiler.PlatformDarwinARM64}}
	_, err = compileHosted(t.Context(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", macOSTarget, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no runtime distribution configured for darwin/arm64") {
		t.Fatalf("missing macOS runtime error = %v", err)
	}
	compiled, err := compileHosted(t.Context(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", macOSTarget, map[compiler.Platform]string{compiler.PlatformDarwinARM64: darwinDigest}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Target.Queue != "macos" || compiled.Bundle.Plans[0].Job.RuntimeDistributionDigest() != darwinDigest || !bytes.Contains(compiled.Bundle.Pipeline, []byte("queue: \"macos\"")) {
		t.Fatalf("macOS compilation = %#v\n%s", compiled.Bundle.Plans, compiled.Bundle.Pipeline)
	}
}

func TestCompileHostedDefaultsMacOSLatestAliasToHostedQueue(t *testing.T) {
	workflow := []byte("on: push\njobs:\n  macos:\n    runs-on: macOS-latest\n    steps:\n      - run: echo macos\n")
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	compilerDigest := "sha256:" + strings.Repeat("0", 64)
	darwinDigest := "sha256:" + strings.Repeat("1", 64)
	_, err = compileHosted(t.Context(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no runtime distribution configured for darwin/arm64") {
		t.Fatalf("unmapped macos-latest without Darwin runtime error = %v", err)
	}
	darwinRuntime := map[compiler.Platform]string{compiler.PlatformDarwinARM64: darwinDigest}
	compiled, err := compileHosted(t.Context(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", nil, darwinRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Target.Queue != defaultMacOSRunnerQueue || compiled.Bundle.Plans[0].Job.RuntimeDistributionDigest() != darwinDigest || !bytes.Contains(compiled.Bundle.Pipeline, []byte("queue: \"macos-medium\"")) {
		t.Fatalf("default macos-latest compilation = %#v\n%s", compiled.Bundle.Plans, compiled.Bundle.Pipeline)
	}
	override := map[string]compiler.RunnerTarget{"macos-latest": {Queue: "custom-macos", Platform: compiler.PlatformDarwinARM64}}
	compiled, err = compileHosted(t.Context(), "macos.yml", workflow, event, "0.0.0-test", compilerDigest, "importer", "", override, darwinRuntime, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Bundle.Plans) != 1 || compiled.Bundle.Plans[0].Job.Target.Queue != "custom-macos" || !bytes.Contains(compiled.Bundle.Pipeline, []byte("queue: \"custom-macos\"")) {
		t.Fatalf("overridden macos-latest compilation = %#v\n%s", compiled.Bundle.Plans, compiled.Bundle.Pipeline)
	}
}

func TestJobScopedActionSourceAuthenticationIgnoresAmbientGitHubTokens(t *testing.T) {
	t.Setenv("GH_TOKEN", "ignored")
	t.Setenv("GITHUB_TOKEN", "ignored")
	t.Setenv("BUILDKITE_GHA_GITHUB_TOKEN", "ignored")
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", "")
	t.Setenv("BUILDKITE_JOB_ID", "")
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "")
	var warnings bytes.Buffer
	authentication := importerJobActionSourceAuthentication(&warnings, "test-version")
	token, err := authentication.token(t.Context(), "buildkite/buildkite-gha")
	if err != nil || token != "" || authentication.provider != nil || !strings.Contains(warnings.String(), "authentication is unavailable") {
		t.Fatalf("ambient GitHub token authentication = provider %v, token %q, error %v, warnings %q", authentication.provider != nil, token, err, warnings.String())
	}

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	t.Setenv("BUILDKITE_AGENT_ENDPOINT", server.URL)
	t.Setenv("BUILDKITE_JOB_ID", cliTestJobID)
	t.Setenv("BUILDKITE_AGENT_ACCESS_TOKEN", "job-token")
	if authentication := importerJobActionSourceAuthentication(io.Discard, "test-version"); authentication.provider == nil {
		t.Fatal("job-scoped Agent configuration did not configure action-source authentication")
	}
}

func TestRunnerProfileDoesNotAffectOtherLinuxLabels(t *testing.T) {
	workflow := []byte(`on: push
jobs:
  selected:
    runs-on: ubuntu-latest
    steps:
      - run: echo selected
  default:
    runs-on: ubuntu-22.04
    steps:
      - run: echo default
`)
	event, err := os.ReadFile(filepath.Join("..", "..", "testdata", "smoke", "events", "push.json"))
	if err != nil {
		t.Fatal(err)
	}
	image := "buildkite.namespace-images.com/agent-base@sha256:" + strings.Repeat("0", 64)
	targets := map[string]compiler.RunnerTarget{
		"ubuntu-latest": {Queue: "hosted", Platform: compiler.PlatformLinuxAMD64, Image: image},
	}
	compiled, err := compileHosted(t.Context(), "profiles.yml", workflow, event, "dev", "sha256:"+strings.Repeat("1", 64), "importer", "", targets, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pipeline struct {
		Steps []struct {
			Key     string            `yaml:"key"`
			Image   string            `yaml:"image"`
			Agents  map[string]string `yaml:"agents"`
			Command string            `yaml:"command"`
		} `yaml:"steps"`
	}
	if err := yaml.Unmarshal(compiled.Bundle.Pipeline, &pipeline); err != nil {
		t.Fatal(err)
	}
	steps := make(map[string]struct {
		Image   string
		Agents  map[string]string
		Command string
	}, len(pipeline.Steps))
	for _, step := range pipeline.Steps {
		steps[step.Key] = struct {
			Image   string
			Agents  map[string]string
			Command string
		}{Image: step.Image, Agents: step.Agents, Command: step.Command}
	}
	selected, defaulted := steps["gha-selected"], steps["gha-default"]
	if selected.Image != image || selected.Agents["queue"] != "hosted" || !strings.Contains(selected.Command, "--hosted-tool-cache") {
		t.Fatalf("selected profile step = %#v", selected)
	}
	if defaulted.Image != defaultJammyRunnerImage || len(defaulted.Agents) != 0 || !strings.Contains(defaulted.Command, "--hosted-tool-cache") {
		t.Fatalf("unmapped Linux step = %#v, want default targeting with the Jammy hosted-toolchains image", defaulted)
	}
}

func TestUnprivilegedUploadRejectsCapabilities(t *testing.T) {
	tests := []struct {
		capability string
		want       string
	}{
		{capability: "privileged-container", want: `Job "protected" requires unsupported hosted runtime capability "privileged-container". Remove the requirement or use a runtime profile that supports it.`},
		{capability: "future-capability", want: `Job "protected" requires unsupported hosted runtime capability "future-capability". Remove the requirement or use a runtime profile that supports it.`},
	}
	for _, test := range tests {
		capability := test.capability
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow:             plan.Workflow{LogicalJobID: "protected"},
			RequiredCapabilities: []string{capability},
		}}}}
		err := validateUnprivilegedBundle(bundle)
		var finding *compiler.ProcessingFinding
		if err == nil || !errors.As(err, &finding) || finding.Message != test.want || finding.Detail != "" {
			t.Fatalf("validateUnprivilegedBundle(%q) error = %v, want capability rejection", capability, err)
		}
	}
}

func TestUnprivilegedUploadAdmitsSecretsCapability(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow:             plan.Workflow{LogicalJobID: "release"},
		RequiredCapabilities: []string{"secrets"},
		RequiredSecrets:      []string{"HOMEBREW_TAP_GITHUB_TOKEN"},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatalf("validateUnprivilegedBundle() error = %v", err)
	}
}

func TestUnprivilegedUploadAdmitsOnlyCompilerVerifiedCheckoutCredentials(t *testing.T) {
	job := plan.Job{
		Workflow:             plan.Workflow{LogicalJobID: "checkout"},
		RequiredCapabilities: []string{"network", "provider-token-read"},
	}
	for _, test := range []struct {
		name          string
		authorization compiler.PlanAuthorization
		wantError     bool
	}{
		{name: "verified adapter", authorization: compiler.PlanAuthorization{ProviderTokenReadCapabilitySources: []string{"checkout-adapter"}}},
		{name: "missing provenance", wantError: true},
		{name: "broadened provenance", authorization: compiler.PlanAuthorization{ProviderTokenReadCapabilitySources: []string{"checkout-adapter", "javascript-action"}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job, Authorization: test.authorization}}}
			err := validateUnprivilegedBundle(bundle)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "unsupported GitHub checkout credentials")) {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
		})
	}
}

func TestUnprivilegedUploadAdmitsOnlyCompilerVerifiedWorkflowToken(t *testing.T) {
	job := plan.Job{
		Workflow:             plan.Workflow{Path: ".github/workflows/comment.yml", LogicalJobID: "comment"},
		RequiredCapabilities: []string{"provider-token-write"},
		GitHubToken:          &plan.GitHubToken{Workflow: "comment.yml", Permissions: map[string]string{"pull_requests": "write"}},
	}
	for _, test := range []struct {
		name          string
		authorization compiler.PlanAuthorization
		wantError     bool
	}{
		{name: "verified policy", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions"}, WorkflowTokenPolicyFilename: "comment.yml"}},
		{name: "missing provenance", wantError: true},
		{name: "missing policy", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions"}}, wantError: true},
		{name: "mismatched policy", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions"}, WorkflowTokenPolicyFilename: "other.yml"}, wantError: true},
		{name: "broadened provenance", authorization: compiler.PlanAuthorization{ProviderTokenWriteCapabilitySources: []string{"effective-permissions", "step-input"}, WorkflowTokenPolicyFilename: "comment.yml"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job, Authorization: test.authorization}}}
			err := validateUnprivilegedBundle(bundle)
			if test.wantError && (err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN")) {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateUnprivilegedBundle() error = %v", err)
			}
		})
	}
}

func TestUnprivilegedUploadAllowsPublicAndDockerfileActionCapabilities(t *testing.T) {
	for _, capabilities := range [][]string{nil, {"network"}, {"docker"}, {"docker", "network"}} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow:             plan.Workflow{LogicalJobID: "action-job"},
			RequiredCapabilities: capabilities,
			Steps:                []plan.Step{{ID: "action", Kind: "uses", Uses: "owner/example@commit"}},
		}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: []string{"dockerfile-actions"}}}}}
		if err := validateUnprivilegedBundle(bundle); err != nil {
			t.Fatalf("validateUnprivilegedBundle(%v) error = %v", capabilities, err)
		}
	}
}

func TestUnprivilegedUploadRejectsDockerWithoutCompilerProvenance(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow:             plan.Workflow{LogicalJobID: "unproven-docker"},
		RequiredCapabilities: []string{"docker"},
	}}}}
	err := validateUnprivilegedBundle(bundle)
	var finding *compiler.ProcessingFinding
	want := `Job "unproven-docker" requires Docker without matching compiler provenance. Hosted runs support only verified Dockerfile actions and bounded job or service containers.`
	if err == nil || !errors.As(err, &finding) || finding.Message != want || finding.Detail != "" {
		t.Fatalf("validateUnprivilegedBundle() error = %v, want Docker provenance rejection", err)
	}
}

func TestUnprivilegedUploadAdmitsCompilerVerifiedContainerProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		job     plan.Job
		sources []string
	}{
		{name: "job", job: plan.Job{Container: &plan.Container{Image: "node:24"}}, sources: []string{"job-containers"}},
		{name: "service", job: plan.Job{Services: map[string]plan.ServiceContainer{"redis": {Image: "redis:7"}}}, sources: []string{"service-containers"}},
		{name: "dynamic services", job: plan.Job{ServicesExpression: "${{ fromJSON(needs.build.outputs.services) }}"}, sources: []string{"service-containers"}},
		{name: "all", job: plan.Job{Container: &plan.Container{Image: "node:24"}, Services: map[string]plan.ServiceContainer{"redis": {Image: "redis:7"}}}, sources: []string{"dockerfile-actions", "job-containers", "service-containers"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "container-job"}, RequiredCapabilities: []string{"docker", "network"}, Container: test.job.Container, Services: test.job.Services, ServicesExpression: test.job.ServicesExpression,
			}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: test.sources}}}}
			if err := validateUnprivilegedBundle(bundle); err != nil {
				t.Fatalf("validateUnprivilegedBundle(%v) error = %v", test.sources, err)
			}
		})
	}
}

func TestUnprivilegedUploadRejectsMismatchedContainerProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		job     plan.Job
		sources []string
	}{
		{name: "claim without container", sources: []string{"job-containers"}},
		{name: "container without claim", job: plan.Job{Container: &plan.Container{Image: "node:24"}}},
		{name: "unknown source", sources: []string{"docker-plugin"}},
		{name: "unsorted source", job: plan.Job{Container: &plan.Container{Image: "node:24"}}, sources: []string{"job-containers", "dockerfile-actions"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "container-job"}, RequiredCapabilities: []string{"docker", "network"}, Container: test.job.Container,
			}, Authorization: compiler.PlanAuthorization{DockerCapabilitySources: test.sources}}}}
			if err := validateUnprivilegedBundle(bundle); err == nil || !strings.Contains(err.Error(), "unsupported Docker access") {
				t.Fatalf("validateUnprivilegedBundle(%v) error = %v", test.sources, err)
			}
		})
	}
}

func TestUnprivilegedUploadRejectsKnownGitHubServiceActions(t *testing.T) {
	tests := []struct {
		action  plan.ActionLock
		service string
	}{
		{plan.ActionLock{Source: "github", Repository: "actions/upload-artifact", Path: "merge"}, "artifact"},
	}
	for _, test := range tests {
		name := test.action.Repository
		if test.action.Path != "" {
			name += "/" + test.action.Path
		}
		t.Run(name, func(t *testing.T) {
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "service-action"},
				Actions:  []plan.ActionLock{test.action},
			}}}}
			err := validateUnprivilegedBundle(bundle)
			var finding *compiler.ProcessingFinding
			want := `Action "actions/upload-artifact/merge" requires the GitHub Actions artifact service, which hosted runs do not provide. Replace the action or use a runtime profile that provides this service.`
			if err == nil || !errors.As(err, &finding) || finding.Message != want || finding.Detail != "" {
				t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", test.action, err)
			}
		})
	}
}

func TestUnprivilegedUploadAllowsOnlyAuditedCacheCommits(t *testing.T) {
	for _, commit := range actionintegration.CacheCommits() {
		for _, path := range []string{"", "restore", "save"} {
			action := plan.ActionLock{Source: "github", Repository: "actions/cache", Path: path, Commit: commit}
			bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
				Workflow: plan.Workflow{LogicalJobID: "cache"},
				Actions:  []plan.ActionLock{action},
			}}}}
			if err := validateUnprivilegedBundle(bundle); err != nil {
				t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
			}
		}
	}

	action := plan.ActionLock{Source: "github", Repository: "actions/cache", RequestedRef: "v6.0.2", Commit: strings.Repeat("0", 40)}
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "cache-old"},
		Actions:  []plan.ActionLock{action},
	}}}}
	err := validateUnprivilegedBundle(bundle)
	if err == nil || !strings.Contains(err.Error(), "v6.1.0") {
		t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
	}
	var finding *compiler.ProcessingFinding
	if !errors.As(err, &finding) || finding.Message != "actions/cache v6.0.2 is unsupported. Use a supported version from https://github.com/buildkite/buildkite-gha/blob/main/docs/compatibility.md#cache-action." || !strings.Contains(finding.Detail, "Buildkite cache-v2 service") || !strings.Contains(finding.Detail, action.Commit) || strings.Contains(finding.Message, action.Commit) {
		t.Fatalf("hosted cache diagnostic = %#v", finding)
	}
}

func TestCacheServiceRequiredUsesOnlyAuditedCacheLocks(t *testing.T) {
	required, err := cacheServiceRequired([]plan.ActionLock{{Source: "github", Repository: "owner/action", Commit: strings.Repeat("a", 40)}})
	if err != nil || required {
		t.Fatalf("ordinary action cache requirement = %v, %v", required, err)
	}

	locks := []plan.ActionLock{{Source: "github", Repository: "owner/action", Commit: strings.Repeat("a", 40)}}
	for _, commit := range actionintegration.CacheCommits() {
		locks = append(locks[:1], plan.ActionLock{Source: "github", Repository: "actions/cache", Path: "restore", Commit: commit})
		required, err = cacheServiceRequired(locks)
		if err != nil || !required {
			t.Fatalf("cache requirement for %s = %v, %v", commit, required, err)
		}
	}

	locks[1].Commit = strings.Repeat("b", 40)
	if _, err := cacheServiceRequired(locks); err == nil || !strings.Contains(err.Error(), actionintegration.CacheCommit) {
		t.Fatalf("unsupported cache lock error = %v", err)
	}
}

func TestUnprivilegedUploadAllowsNativeDownloadArtifactAdapter(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{Workflow: plan.Workflow{LogicalJobID: "consumer"}, Actions: []plan.ActionLock{{Source: "github", Repository: "actions/download-artifact", Commit: actionintegration.DownloadArtifactCommit}}}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatal(err)
	}
}

func TestUnprivilegedUploadAllowsNativeUploadArtifactAdapter(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "artifact-producer"},
		Actions: []plan.ActionLock{{
			Source: "github", Repository: "actions/upload-artifact",
			Commit: actionintegration.UploadArtifactCommit,
		}},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatalf("validateUnprivilegedBundle() error = %v", err)
	}
}

func TestUnprivilegedUploadDoesNotBroadenKnownServiceActionIdentity(t *testing.T) {
	for _, action := range []plan.ActionLock{
		{Source: "workspace", Repository: "actions/cache"},
		{Source: "github", Repository: "actions/cache", Path: "nested"},
		{Source: "github", Repository: "actions/upload-artifact", Path: "nested"},
		{Source: "github", Repository: "actions/download-artifact", Path: "nested"},
		{Source: "github", Repository: "owner/action"},
	} {
		bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
			Workflow: plan.Workflow{LogicalJobID: "ordinary-action"},
			Actions:  []plan.ActionLock{action},
		}}}}
		if err := validateUnprivilegedBundle(bundle); err != nil {
			t.Fatalf("validateUnprivilegedBundle(%#v) error = %v", action, err)
		}
	}
}

func TestBundleUsesActionsDetectsStepsAndLocks(t *testing.T) {
	for _, job := range []plan.Job{
		{Steps: []plan.Step{{Kind: "uses"}}},
		{Actions: []plan.ActionLock{{ID: "a-deadbeefdeadbeef"}}},
	} {
		if !bundleUsesActions(compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: job}}}) {
			t.Fatalf("bundleUsesActions() = false for %#v", job)
		}
	}
	if bundleUsesActions(compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{Steps: []plan.Step{{Kind: "run"}}}}}}) {
		t.Fatal("bundleUsesActions() = true for shell-only plan")
	}
}

func TestUnprivilegedUploadAllowsCapabilityFreeConcurrentShellSteps(t *testing.T) {
	bundle := compiler.Bundle{Plans: []compiler.PlanArtifact{{Job: plan.Job{
		Workflow: plan.Workflow{LogicalJobID: "concurrent-job"},
		Steps: []plan.Step{
			{ID: "background", Kind: "run", Command: "true", Background: true},
			{ID: "wait", Kind: "wait", Targets: []string{"background"}},
			{ID: "wait-all", Kind: "wait-all"},
			{ID: "cancel", Kind: "cancel", Targets: []string{"background"}},
		},
	}}}}
	if err := validateUnprivilegedBundle(bundle); err != nil {
		t.Fatalf("validateUnprivilegedBundle() error = %v", err)
	}
}
