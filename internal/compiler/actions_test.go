package compiler

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	actionintegration "github.com/buildkite/buildkite-gha/internal/action/integration"
	"github.com/buildkite/buildkite-gha/internal/action/metadata"
	"github.com/buildkite/buildkite-gha/internal/action/source"
	"github.com/buildkite/buildkite-gha/internal/plan"
)

type fakeActionSource struct {
	root   string
	calls  map[string]int
	commit string
}

type contextActionSource struct{}

func (contextActionSource) Fetch(ctx context.Context, _ source.Reference) (source.Resolved, source.Materialized, error) {
	<-ctx.Done()
	return source.Resolved{}, source.Materialized{}, ctx.Err()
}

type classifiedActionSource struct {
	roots   map[string]string
	commits map[string]string
}

func (s classifiedActionSource) Fetch(_ context.Context, ref source.Reference) (source.Resolved, source.Materialized, error) {
	repository := strings.ToLower(ref.Owner + "/" + ref.Repository)
	root := s.roots[repository]
	return source.Resolved{Reference: ref, Commit: s.commits[repository]}, source.Materialized{
		RepositoryRoot: root,
		ActionRoot:     filepath.Join(root, ref.Path),
		SourceDigest:   "sha256:" + strings.Repeat("a", 64),
	}, nil
}

func (f *fakeActionSource) Fetch(_ context.Context, r source.Reference) (source.Resolved, source.Materialized, error) {
	f.calls[r.Raw]++
	d, err := source.DigestTree(filepath.Join(f.root, r.Path))
	commit := strings.Repeat("a", 40)
	if f.commit != "" {
		commit = f.commit
	} else if len(r.Ref) == 40 && strings.Trim(strings.ToLower(r.Ref), "0123456789abcdef") == "" {
		commit = strings.ToLower(r.Ref)
	}
	return source.Resolved{Reference: r, Commit: commit}, source.Materialized{RepositoryRoot: f.root, ActionRoot: filepath.Join(f.root, r.Path), SourceDigest: d}, err
}

func writeAction(t *testing.T, root, name, body string) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "action.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "using: docker") {
		if err := os.WriteFile(filepath.Join(d, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []string{"index.js"} {
		if strings.Contains(body, entry) {
			if err := os.WriteFile(filepath.Join(d, entry), []byte("// fixture\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCompileActionLocksRequiresMiseOnlyForJavaScriptReachableGraphs(t *testing.T) {
	workspace := t.TempDir()
	writeAction(t, workspace, "docker", "runs:\n  using: docker\n  image: Dockerfile\n")
	writeAction(t, workspace, "native-docker-composite", "runs:\n  using: composite\n  steps:\n    - run: echo native\n      shell: sh\n    - uses: actions/checkout@v4\n    - uses: ./docker\n")
	writeAction(t, workspace, "js", "runs:\n  using: node24\n  main: index.js\n")
	writeAction(t, workspace, "js-composite", "runs:\n  using: composite\n  steps:\n    - uses: ./js\n")

	remote := func(repository, using string) string {
		root := t.TempDir()
		writeAction(t, root, "", "runs:\n  using: "+using+"\n  main: index.js\n")
		return root
	}
	remoteComposite := t.TempDir()
	writeAction(t, remoteComposite, "", "runs:\n  using: composite\n  steps:\n    - uses: owner/javascript@v1\n")
	source := classifiedActionSource{
		roots: map[string]string{
			"actions/checkout":          remote("actions/checkout", "node24"),
			"actions/upload-artifact":   remote("actions/upload-artifact", "node24"),
			"actions/download-artifact": remote("actions/download-artifact", "node24"),
			"actions/cache":             remote("actions/cache", "node24"),
			"owner/composite":           remoteComposite,
			"owner/javascript":          remote("owner/javascript", "node24"),
		},
		commits: map[string]string{
			"actions/checkout":          actionintegration.CheckoutV4Commit,
			"actions/upload-artifact":   actionintegration.UploadArtifactCommit,
			"actions/download-artifact": actionintegration.DownloadArtifactCommit,
			"actions/cache":             actionintegration.CacheCommit,
			"owner/composite":           strings.Repeat("c", 40),
			"owner/javascript":          strings.Repeat("d", 40),
		},
	}

	tests := []struct {
		name string
		refs []string
		want bool
	}{
		{name: "shell only"},
		{name: "native checkout only", refs: []string{"actions/checkout@v4"}},
		{name: "native upload only", refs: []string{"actions/upload-artifact@v4"}},
		{name: "native download only", refs: []string{"actions/download-artifact@v4"}},
		{name: "Docker only", refs: []string{"./docker"}},
		{name: "native and Docker composite", refs: []string{"./native-docker-composite"}},
		{name: "JavaScript", refs: []string{"./js"}, want: true},
		{name: "JavaScript through composite", refs: []string{"./js-composite"}, want: true},
		{name: "JavaScript through remote composite", refs: []string{"owner/composite@v1"}, want: true},
		{name: "cache v2 client", refs: []string{"actions/cache@v6.1.0"}, want: true},
		{name: "mixed Docker and JavaScript", refs: []string{"./docker", "./js"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, got, err := compileActionLocks(context.Background(), workspace, source, test.refs)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("requires mise = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCompileActionInvocationsValidatesNestedUploadArtifact(t *testing.T) {
	workspace, upload := t.TempDir(), t.TempDir()
	writeAction(t, upload, "", "name: upload artifact\nruns:\n  using: node20\n  main: index.js\n")
	source := classifiedActionSource{
		roots:   map[string]string{"actions/upload-artifact": upload},
		commits: map[string]string{"actions/upload-artifact": actionintegration.UploadArtifactCommit},
	}

	writeAction(t, workspace, "parent", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: actions/upload-artifact@v4\n      with:\n        path: payload\n        overwrite: true\n")
	if _, err := compileActionInvocations(context.Background(), workspace, source, []string{"./parent"}, []map[string]string{{}}); err == nil || !strings.Contains(err.Error(), "bounded upload-artifact adapter") || !strings.Contains(err.Error(), "overwrite") {
		t.Fatalf("nested literal validation error = %v", err)
	}

	writeAction(t, workspace, "parent", "name: parent\ninputs:\n  path:\n    required: true\nruns:\n  using: composite\n  steps:\n    - uses: actions/upload-artifact@v4\n      with:\n        path: ${{ inputs.path }}\n")
	if _, err := compileActionInvocations(context.Background(), workspace, source, []string{"./parent"}, []map[string]string{{"path": "payload"}}); err != nil {
		t.Fatalf("nested expression was not deferred to runtime: %v", err)
	}
}

func TestCompileActionLocksLocalAndDedup(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "js", "name: js\nruns:\n  using: node20\n  main: index.js\n")
	selectors, locks, caps, _, err := compileActionLocks(context.Background(), w, nil, []string{"./js", "./js"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || len(selectors) != 2 || selectors[0] != selectors[1] || locks[0].Path != "js" || !strings.HasPrefix(locks[0].SourceDigest, "sha256:") || len(caps) != 0 {
		t.Fatalf("unexpected result: %#v %#v %#v", selectors, locks, caps)
	}
}

func TestCompileActionInvocationsDetectsEffectiveGitHubTokenDefaults(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "token", `name: token default
inputs:
  github_token:
    default: ${{ github.token }}
runs:
  using: node24
  main: index.js
`)
	writeAction(t, w, "actor", `name: actor default
inputs:
  actor:
    default: ${{ github.actor }}
runs:
  using: node24
  main: index.js
`)
	writeAction(t, w, "dynamic", `name: dynamic default
inputs:
  value:
    default: ${{ github[env.NAME] }}
runs:
  using: node24
  main: index.js
`)
	writeAction(t, w, "mixed", `name: mixed defaults
inputs:
  a_token:
    default: ${{ github.token }}
  z_dynamic:
    default: ${{ github[env.NAME] }}
runs:
  using: node24
  main: index.js
`)
	writeAction(t, w, "complex", `name: complex default
inputs:
  token:
    default: ${{ github.server_url == 'https://github.com' && github.token || '' }}
runs:
  using: node24
  main: index.js
`)

	for _, test := range []struct {
		name     string
		ref      string
		supplied map[string]string
		want     bool
		wantErr  string
	}{
		{name: "effective default", ref: "./token", want: true},
		{name: "explicit empty input", ref: "./token", supplied: map[string]string{"github_token": ""}},
		{name: "case-insensitive explicit input", ref: "./token", supplied: map[string]string{"GITHUB_TOKEN": "explicit"}},
		{name: "unrelated GitHub default", ref: "./actor"},
		{name: "conditional token default", ref: "./complex", want: true},
		{name: "explicit input bypasses default", ref: "./complex", supplied: map[string]string{"token": ""}},
		{name: "dynamic GitHub index", ref: "./dynamic", wantErr: "index must be a string literal"},
		{name: "token before dynamic GitHub index", ref: "./mixed", wantErr: "index must be a string literal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := compileActionInvocations(context.Background(), w, nil, []string{test.ref}, []map[string]string{test.supplied})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("compileActionInvocations() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if compiled.requiresGitHubToken != test.want {
				t.Fatalf("requires GitHub token = %t, want %t", compiled.requiresGitHubToken, test.want)
			}
		})
	}
}

func TestCompileActionInvocationsScopesWorkflowAuthoredSecretsByInputContract(t *testing.T) {
	workspace := t.TempDir()
	writeAction(t, workspace, "secrets", `name: secret inputs
inputs:
  optional:
    default: ""
  required:
    required: true
runs:
  using: node24
  main: index.js
`)
	compiled, err := compileActionInvocations(
		context.Background(), workspace, nil, []string{"./secrets"},
		[]map[string]string{{"optional": "${{ secrets.OPTIONAL_TOKEN }}", "required": "${{ secrets.REQUIRED_TOKEN }}-${{ secrets.GITHUB_TOKEN }}"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled.requiredSecrets, []string{"GITHUB_TOKEN", "REQUIRED_TOKEN"}) {
		t.Fatalf("required action secrets = %#v", compiled.requiredSecrets)
	}
}

func TestCompileActionInvocationsRejectsSecretAuthorityFromCompositeMetadata(t *testing.T) {
	workspace := t.TempDir()
	writeAction(t, workspace, "child", `name: child
inputs:
  token:
    required: true
runs:
  using: node24
  main: index.js
`)
	writeAction(t, workspace, "parent", `name: parent
runs:
  using: composite
  steps:
    - uses: ./child
      with:
        token: ${{ secrets.DEPLOY_KEY }}
`)

	_, err := compileActionInvocations(context.Background(), workspace, nil, []string{"./parent"}, []map[string]string{nil})
	if err == nil || !strings.Contains(err.Error(), "composite action metadata cannot grant secret authority") {
		t.Fatalf("composite metadata secret error = %v", err)
	}
}

func TestCompileActionInvocationsRejectsSecretAuthorityFromMetadataDefaults(t *testing.T) {
	workspace := t.TempDir()
	writeAction(t, workspace, "secrets", `name: secret default
inputs:
  token:
    default: ${{ secrets.DEPLOY_KEY }}
runs:
  using: node24
  main: index.js
`)
	_, err := compileActionInvocations(context.Background(), workspace, nil, []string{"./secrets"}, []map[string]string{nil})
	if err == nil || !strings.Contains(err.Error(), "action input defaults cannot grant secret authority") {
		t.Fatalf("metadata secret default error = %v", err)
	}
}

func TestCompileActionInvocationsAcceptsResolvedRemoteConditionalDefaults(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	writeAction(t, remote, "", `name: complex remote default
inputs:
  token:
    default: ${{ github.server_url == 'https://github.com' && github.token || '' }}
runs:
  using: node24
  main: index.js
`)
	actionSource := &fakeActionSource{root: remote, calls: map[string]int{}}
	compiled, err := compileActionInvocations(context.Background(), workspace, actionSource, []string{"owner/action@v1"}, []map[string]string{nil})
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.requiresGitHubToken {
		t.Fatal("conditional remote action default did not require a GitHub token")
	}
	if actionSource.calls["owner/action@v1"] != 1 {
		t.Fatalf("remote action resolutions = %#v, want one", actionSource.calls)
	}
}

func TestCompileActionInvocationsDetectsOnlyEffectiveNestedGitHubTokenDefaults(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "child", `name: child
inputs:
  github_token:
    default: ${{ github.token }}
runs:
  using: node24
  main: index.js
`)
	writeAction(t, w, "parent", `name: parent
runs:
  using: composite
  steps:
    - uses: ./child
`)
	writeAction(t, w, "overridden", `name: overridden parent
runs:
  using: composite
  steps:
    - uses: ./child
      with:
        github_token: ''
`)
	writeAction(t, w, "dynamic-child", `name: dynamic child
inputs:
  value:
    default: ${{ github[env.NAME] }}
runs:
  using: node24
  main: index.js
`)
	writeAction(t, w, "mixed-parent", `name: mixed parent
runs:
  using: composite
  steps:
    - uses: ./child
    - uses: ./dynamic-child
`)

	for _, test := range []struct {
		ref  string
		want bool
	}{
		{ref: "./parent", want: true},
		{ref: "./overridden"},
	} {
		compiled, err := compileActionInvocations(context.Background(), w, nil, []string{test.ref}, []map[string]string{nil})
		if err != nil {
			t.Fatal(err)
		}
		if compiled.requiresGitHubToken != test.want {
			t.Fatalf("%s requires GitHub token = %t, want %t", test.ref, compiled.requiresGitHubToken, test.want)
		}
	}
	if _, err := compileActionInvocations(context.Background(), w, nil, []string{"./mixed-parent"}, []map[string]string{nil}); err == nil || !strings.Contains(err.Error(), "index must be a string literal") {
		t.Fatalf("mixed child traversal error = %v, want dynamic index rejection", err)
	}
}

func TestCompilePlansScopesGitHubTokenForEffectiveActionDefaults(t *testing.T) {
	w := t.TempDir()
	workflowPath := filepath.Join(w, ".github", "workflows", "token.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, w, ".github/actions/token", `name: token default
inputs:
  github_token:
    default: ${{ github.token }}
runs:
  using: node24
  main: index.js
`)
	compile := func(workflow string) ([]plan.Job, error) {
		if err := os.WriteFile(workflowPath, []byte(workflow), 0o644); err != nil {
			t.Fatal(err)
		}
		return compilePlansForTest(context.Background(), workflowPath, []byte(workflow), pushEvent(t), "0.0.0-test", testDistributionDigest, defaultOptions())
	}

	effectiveWorkflow := `on: push
permissions:
  contents: read
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token
`
	plans, err := compile(effectiveWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil || !reflect.DeepEqual(plans[0].GitHubToken.Permissions, map[string]string{"contents": "read"}) || !plans[0].HasCapability("provider-token-write") {
		t.Fatalf("effective action default token plan = %#v", plans)
	}
	bundle, err := CompileBundleWithOptions(workflowPath, []byte(effectiveWorkflow), pushEvent(t), "0.0.0-test", testDistributionDigest, "importer", defaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 1 || !reflect.DeepEqual(bundle.Plans[0].Authorization.ProviderTokenWriteCapabilitySources, []string{"effective-permissions"}) {
		t.Fatalf("effective action default token authorization = %#v", bundle.Plans)
	}

	plans, err = compile(`on: push
permissions:
  contents: read
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil || !reflect.DeepEqual(plans[0].GitHubToken.Permissions, map[string]string{"contents": "read"}) || len(plans[0].RequiredSecrets) != 0 {
		t.Fatalf("explicit optional action token plan = %#v", plans)
	}

	plans, err = compile(`on: push
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token
        with:
          GITHUB_TOKEN: ''
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken != nil || plans[0].HasCapability("provider-token-write") {
		t.Fatalf("overridden action default minted a token: %#v", plans)
	}

	plans, err = compile(`on: push
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].GitHubToken == nil || !reflect.DeepEqual(plans[0].GitHubToken.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("default action token plan = %#v", plans)
	}

	_, err = compile(`on: push
permissions: {}
jobs:
  token:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/token
`)
	if err == nil || !strings.Contains(err.Error(), "action input default that references github.token") || !strings.Contains(err.Error(), "no effective permissions") {
		t.Fatalf("compilePlansForTest() error = %v, want empty permission rejection", err)
	}
}

func TestCompileActionLocksRemoteCompositeUsesWorkspaceRoot(t *testing.T) {
	w, remote := t.TempDir(), t.TempDir()
	writeAction(t, w, "child", "name: child\nruns:\n  using: docker\n  image: Dockerfile\n")
	writeAction(t, remote, "", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	f := &fakeActionSource{root: remote, calls: map[string]int{}}
	_, locks, caps, _, err := compileActionLocks(context.Background(), w, f, []string{"Owner/Repo@v1", "Owner/Repo@v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || f.calls["Owner/Repo@v1"] != 1 || !reflect.DeepEqual(caps, []string{"docker", "network"}) {
		t.Fatalf("unexpected result: %#v calls=%v caps=%v", locks, f.calls, caps)
	}
	var found bool
	for _, l := range locks {
		if l.Source == "workspace" && l.Path == "child" {
			found = true
		}
	}
	if !found {
		t.Fatal("local child was not locked from workspace")
	}
}

func TestCompileActionLocksRecursion(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "loop", "name: loop\nruns:\n  using: composite\n  steps:\n    - uses: ./loop\n")
	_, _, _, _, err := compileActionLocks(context.Background(), w, nil, []string{"./loop"})
	if err == nil || !strings.Contains(err.Error(), "recursion") {
		t.Fatalf("got %v", err)
	}
}

func TestCompileActionLocksRequiresRepositoryRoot(t *testing.T) {
	_, _, _, _, err := compileActionLocks(context.Background(), "", nil, []string{"./local"})
	if err == nil || !strings.Contains(err.Error(), "workflow path must identify a repository root") {
		t.Fatalf("compileActionLocks() error = %v, want repository-root rejection", err)
	}
}

func TestCompileActionLocksRejectsExcessiveDepthBeforeResolvingLeaf(t *testing.T) {
	w := t.TempDir()
	for i := 0; i <= metadata.MaxNestedActionDepth; i++ {
		steps := ""
		if i < metadata.MaxNestedActionDepth {
			steps = "  steps:\n    - uses: ./depth-" + strconv.Itoa(i+1) + "\n"
		}
		writeAction(t, w, "depth-"+strconv.Itoa(i), "name: depth\nruns:\n  using: composite\n"+steps)
	}
	_, _, _, _, err := compileActionLocks(context.Background(), w, nil, []string{"./depth-0"})
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum depth") {
		t.Fatalf("compileActionLocks() error = %v, want depth rejection", err)
	}
}

func TestCompileActionLocksRejectsEscapedJavaScriptEntrypoint(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "js", "name: js\nruns:\n  using: node20\n  main: ../outside.js\n")
	if err := os.WriteFile(filepath.Join(w, "outside.js"), []byte("// outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, err := compileActionLocks(context.Background(), w, nil, []string{"./js"})
	if err == nil || !strings.Contains(err.Error(), "escapes action source") {
		t.Fatalf("compileActionLocks() error = %v, want entry-point confinement rejection", err)
	}
}

func TestCompileActionLocksExplicitRemoteAndDistinctRefs(t *testing.T) {
	w, remote := t.TempDir(), t.TempDir()
	writeAction(t, remote, "", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: Other/Child/sub@v2\n")
	writeAction(t, remote, "sub", "name: child\nruns:\n  using: node24\n  main: index.js\n")
	f := &fakeActionSource{root: remote, calls: map[string]int{}}
	selectors, locks, _, _, err := compileActionLocks(context.Background(), w, f, []string{"Owner/Repo@v1", "Owner/Repo@main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 3 || selectors[0] == selectors[1] || f.calls["Other/Child/sub@v2"] != 1 {
		t.Fatalf("unexpected result: selectors=%v locks=%#v calls=%v", selectors, locks, f.calls)
	}
	for _, lock := range locks {
		if lock.Source == "github" && lock.Repository != strings.ToLower(lock.Repository) {
			t.Fatalf("repository is not canonical: %q", lock.Repository)
		}
	}
}

func TestCompileActionLocksDeterministic(t *testing.T) {
	w := t.TempDir()
	writeAction(t, w, "parent", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	writeAction(t, w, "child", "name: child\nruns:\n  using: node20\n  main: index.js\n")
	aSelectors, aLocks, aCaps, _, err := compileActionLocks(context.Background(), w, nil, []string{"./parent"})
	if err != nil {
		t.Fatal(err)
	}
	bSelectors, bLocks, bCaps, _, err := compileActionLocks(context.Background(), w, nil, []string{"./parent"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]any{aSelectors, aLocks, aCaps}, []any{bSelectors, bLocks, bCaps}) {
		t.Fatalf("non-deterministic output:\n%#v\n%#v", aLocks, bLocks)
	}
}

func TestCompileActionLocksAllowsOnlyAuditedCacheCommits(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	for _, path := range []string{"", "restore", "save"} {
		writeAction(t, remote, path, "name: cache\nruns:\n  using: node24\n  main: index.js\n")
	}
	for version, commit := range map[string]string{
		"v5.0.3": actionintegration.CacheV503Commit,
		"v5.1.0": actionintegration.CacheV5Commit,
		"v6.1.0": actionintegration.CacheCommit,
	} {
		for _, path := range []string{"", "restore", "save"} {
			name := version + "/root"
			if path != "" {
				name = version + "/" + path
			}
			t.Run(name, func(t *testing.T) {
				uses := "actions/cache"
				if path != "" {
					uses += "/" + path
				}
				uses += "@" + commit
				_, locks, capabilities, _, err := compileActionLocks(context.Background(), workspace, &fakeActionSource{root: remote, calls: map[string]int{}, commit: commit}, []string{uses})
				if err != nil {
					t.Fatal(err)
				}
				if len(locks) != 1 || locks[0].Commit != commit || !reflect.DeepEqual(capabilities, []string{"network"}) {
					t.Fatalf("cache locks/capabilities = %#v / %#v", locks, capabilities)
				}
			})
		}
	}

	_, locks, _, _, err := compileActionLocks(context.Background(), workspace, &fakeActionSource{root: remote, calls: map[string]int{}, commit: actionintegration.CacheCommit}, []string{"actions/cache@v6.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || locks[0].RequestedRef != "v6.1.0" || locks[0].Commit != actionintegration.CacheCommit {
		t.Fatalf("version-ref cache lock = %#v", locks)
	}

	resolved := strings.Repeat("a", 40)
	_, _, _, _, err = compileActionLocks(context.Background(), workspace, &fakeActionSource{root: remote, calls: map[string]int{}}, []string{"actions/cache@v6"})
	if err == nil || !strings.Contains(err.Error(), "actions/cache@v6 resolved to commit "+resolved) || !strings.Contains(err.Error(), actionintegration.CacheV503Commit) || !strings.Contains(err.Error(), actionintegration.CacheV5Commit) || !strings.Contains(err.Error(), actionintegration.CacheCommit) {
		t.Fatalf("unsupported actions/cache commit error = %v", err)
	}
	_, _, _, _, err = compileActionLocks(context.Background(), workspace, &fakeActionSource{root: remote, calls: map[string]int{}}, []string{"actions/cache@v5"})
	if err == nil || !strings.Contains(err.Error(), "actions/cache@v5 resolved to commit "+resolved) {
		t.Fatalf("moved actions/cache v5 error = %v", err)
	}
}

func TestPublicActionSourceNil(t *testing.T) {
	_, _, err := (PublicActionSource{}).Fetch(context.Background(), source.Reference{})
	if err == nil {
		t.Fatal("nil dependencies accepted")
	}
}

func TestCompilePlansTokenlessActionsEmitV3Locks(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, workspace, "local", "name: local\nruns:\n  using: node20\n  main: index.js\n")
	writeAction(t, workspace, "child", "name: child\nruns:\n  using: node24\n  main: index.js\n")
	writeAction(t, remote, "", "name: remote\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	workflow := []byte(`on: push
jobs:
  shell:
    runs-on: ubuntu-latest
    steps:
      - run: echo shell
  actions:
    runs-on: ubuntu-latest
    steps:
      - uses: ./local
      - uses: Owner/Repo@v1
      - uses: Owner/Repo@v1
  other-actions:
    runs-on: ubuntu-latest
    steps:
      - uses: owner/repo@v1
`)
	fake := &fakeActionSource{root: remote, calls: map[string]int{}}
	options := Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   fake,
	}
	first, err := compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("tokenless action plans are not deterministic")
	}
	if len(first) != 3 || first[0].Schema != plan.Schema || first[1].Schema != plan.Schema || first[2].Schema != plan.Schema {
		t.Fatalf("plan schemas = %#v, want current plans", []string{first[0].Schema, first[1].Schema, first[2].Schema})
	}
	actionJob := first[0]
	if len(actionJob.Actions) != 3 || actionJob.Steps[0].Action == nil || actionJob.Steps[1].Action == nil || actionJob.Steps[2].Action == nil || *actionJob.Steps[1].Action != *actionJob.Steps[2].Action {
		t.Fatalf("action locks/selectors = %#v / %#v", actionJob.Actions, actionJob.Steps)
	}
	if fake.calls["Owner/Repo@v1"] != 2 {
		t.Fatalf("remote calls = %d, want one per independent compilation", fake.calls["Owner/Repo@v1"])
	}
	if err := actionJob.Validate(); err != nil {
		t.Fatalf("compiled plan: %v", err)
	}
	var remoteLock *plan.ActionLock
	for i := range actionJob.Actions {
		if actionJob.Actions[i].Source == "github" {
			remoteLock = &actionJob.Actions[i]
			break
		}
	}
	if remoteLock == nil || remoteLock.Repository != "owner/repo" || remoteLock.Commit != strings.Repeat("a", 40) {
		t.Fatalf("remote lock = %#v", remoteLock)
	}
	child := remoteLock.Children["./child"]
	var childLock *plan.ActionLock
	for i := range actionJob.Actions {
		if actionJob.Actions[i].ID == child.Lock {
			childLock = &actionJob.Actions[i]
		}
	}
	if childLock == nil || childLock.Source != "workspace" || childLock.Path != "child" {
		t.Fatalf("remote composite child lock = %#v", childLock)
	}
}

func TestCompilePlansLocksLocalActionsWithoutRemoteResolution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, workspace, "local", "name: local\nruns:\n  using: node24\n  main: index.js\n")
	workflow := []byte(`on: push
jobs:
  actions:
    runs-on: ubuntu-latest
    container: node:24
    services:
      redis:
        image: redis:7
    steps:
      - uses: ./local
`)
	first, err := compileUntrustedPlans(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	second, err := compileUntrustedPlans(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("workspace action plans are not deterministic")
	}
	if len(first) != 1 || first[0].Schema != plan.Schema || len(first[0].Actions) != 1 || first[0].Actions[0].Source != "workspace" || first[0].Steps[0].Action == nil {
		t.Fatalf("workspace action plan = %#v", first)
	}
}

func TestCompilePlansContainersWithRemoteActionsRequireResolution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  actions:\n    runs-on: ubuntu-latest\n    container: node:24\n    steps:\n      - uses: owner/action@v1\n")
	_, err := compileUntrustedPlans(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-untrusted")
	if err == nil || !strings.Contains(err.Error(), "containers with remote actions require action resolution through upload or profile validation") {
		t.Fatalf("compileUntrustedPlans() error = %v", err)
	}
}

func TestCheckoutAdapterInputBoundary(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "checkout.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, remote, "", "name: checkout\nruns:\n  using: node24\n  main: index.js\n")
	options := Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
	}
	compile := func(with string) ([]plan.Job, error) {
		workflow := []byte("on: push\njobs:\n  checkout:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1\n" + with)
		if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
			t.Fatal(err)
		}
		return compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "checkout-test", testDistributionDigest, options)
	}

	accepted := []string{
		"",
		"        with:\n          repository: buildkite/buildkite-gha\n          ref: 1111111111111111111111111111111111111111\n          fetch-depth: '1'\n          persist-credentials: false\n          clean: true\n          set-safe-directory: true\n",
		"        with:\n          fetch-depth: '0'\n",
		"        with:\n          fetch-depth: '100'\n",
		"        with:\n          ref: ${{ github.sha }}\n",
		"        with:\n          ref: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		"        with:\n          ref: test-catalog\n          path: test-catalog\n          fetch-depth: '100'\n          persist-credentials: false\n",
		"        with:\n          submodules: ' ReCuRsIvE '\n",
	}
	for _, with := range accepted {
		plans, err := compile(with)
		if err != nil {
			t.Fatalf("checkout with %q: %v", with, err)
		}
		if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"network", "provider-token-read"}) {
			t.Fatalf("checkout plans = %#v", plans)
		}
	}

	rejected := map[string]string{
		"token":       "          token: ''\n",
		"repository":  "          repository: other/repository\n",
		"ref":         "          ref: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		"ssh-key":     "          ssh-key: key\n",
		"submodules":  "          submodules: yes\n",
		"path":        "          path: nested/path\n",
		"fetch-depth": "          fetch-depth: '-1'\n",
		"credentials": "          persist-credentials: true\n",
	}
	for name, input := range rejected {
		t.Run(name, func(t *testing.T) {
			_, err := compile("        with:\n" + input)
			if err == nil || !strings.Contains(err.Error(), "checkout.yml:") || !strings.Contains(err.Error(), "checkout adapter") {
				t.Fatalf("compilePlansForTest() error = %v", err)
			}
		})
	}
}

func TestCheckoutAdapterCommitBoundary(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	writeAction(t, remote, "", "name: checkout\nruns:\n  using: node24\n  main: index.js\n")
	for version, commit := range map[string]string{
		"v4":     actionintegration.CheckoutV4Commit,
		"v5":     actionintegration.CheckoutV5Commit,
		"v6":     actionintegration.CheckoutV6Commit,
		"v7.0.0": actionintegration.CheckoutV7InitialCommit,
		"v7.0.1": actionintegration.CheckoutV7Commit,
	} {
		t.Run(version, func(t *testing.T) {
			actionSource := &fakeActionSource{root: remote, commit: commit, calls: map[string]int{}}
			_, locks, _, _, err := compileActionLocks(context.Background(), workspace, actionSource, []string{"actions/checkout@" + version})
			if err != nil || len(locks) != 1 || locks[0].Commit != commit {
				t.Fatalf("compileActionLocks() locks = %#v, error = %v", locks, err)
			}
		})
	}

	unknown := strings.Repeat("0", 40)
	actionSource := &fakeActionSource{root: remote, commit: unknown, calls: map[string]int{}}
	if _, _, _, _, err := compileActionLocks(context.Background(), workspace, actionSource, []string{"actions/checkout@v7"}); err == nil || !strings.Contains(err.Error(), "does not admit") {
		t.Fatalf("unknown checkout commit error = %v", err)
	}
}

func TestCheckoutCapabilityRequiresVerifiedRootAdapter(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "checkout.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, remote, "", "name: checkout\nruns:\n  using: node24\n  main: index.js\n")
	compile := func(uses, with string) (Bundle, error) {
		workflow := []byte("on: push\njobs:\n  checkout:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: " + uses + "\n" + with)
		if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
			t.Fatal(err)
		}
		return CompileBundleWithOptions(workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, "importer", Options{
			EventTrust: EventUntrusted,
			Runners: RunnerPolicy{
				Labels:          map[string]string{"ubuntu-latest": "hosted"},
				UntrustedQueues: []string{"hosted"},
			},
			ResolveActions: true,
			ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
		})
	}

	bundle, err := compile("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 1 || !reflect.DeepEqual(bundle.Plans[0].Job.RequiredCapabilities, []string{"network", "provider-token-read"}) ||
		!reflect.DeepEqual(bundle.Plans[0].Authorization.ProviderTokenReadCapabilitySources, []string{"checkout-adapter"}) {
		t.Fatalf("checkout plan = %#v", bundle.Plans)
	}

	bundle, err = compile("owner/action@v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bundle.Plans[0].Job.RequiredCapabilities, []string{"network"}) || len(bundle.Plans[0].Authorization.ProviderTokenReadCapabilitySources) != 0 {
		t.Fatalf("ordinary action received checkout authority = %#v", bundle.Plans[0])
	}

	_, err = compile("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "        with:\n          token: '${{ github.token }}'\n")
	if err == nil || !strings.Contains(err.Error(), "checkout adapter") {
		t.Fatalf("workflow token input error = %v", err)
	}
}

func TestUploadArtifactAdapterInputAndCommitBoundary(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "artifact.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, remote, "", "name: upload artifact\nruns:\n  using: node24\n  main: dist/index.js\n")
	if err := os.MkdirAll(filepath.Join(remote, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, "dist", "index.js"), []byte("throw new Error('adapter only')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	compile := func(commit, with string) ([]plan.Job, error) {
		workflow := []byte("on: push\njobs:\n  upload:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/upload-artifact@" + commit + "\n" + with)
		if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
			t.Fatal(err)
		}
		return compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "artifact-test", testDistributionDigest, Options{
			EventTrust: EventUntrusted,
			Runners: RunnerPolicy{
				Labels:          map[string]string{"ubuntu-latest": "hosted"},
				UntrustedQueues: []string{"hosted"},
			},
			ResolveActions: true,
			ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
		})
	}

	plans, err := compile(actionintegration.UploadArtifactCommit, "        with:\n          name: payload\n          path: payload/result.txt\n          if-no-files-found: error\n          compression-level: '0'\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"network"}) {
		t.Fatalf("upload-artifact plans = %#v", plans)
	}
	plans, err = compile(actionintegration.UploadArtifactV7Commit, "        with:\n          name: payload\n          path: payload/result.txt\n          archive: true\n")
	if err != nil {
		t.Fatalf("audited v7 ZIP upload rejected: %v", err)
	}
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"network"}) {
		t.Fatalf("upload-artifact v7 plans = %#v", plans)
	}
	for version, commit := range map[string]string{
		"v5.0.0": actionintegration.UploadArtifactV5Commit,
		"v6.0.0": actionintegration.UploadArtifactV6Commit,
	} {
		if _, err := compile(commit, "        with:\n          name: ${{ github.sha }}\n          path: ./payload/result.txt\n          retention-days: '0'\n"); err != nil {
			t.Fatalf("audited %s ZIP upload rejected: %v", version, err)
		}
	}
	if _, err := compile(actionintegration.UploadArtifactCommit, "        with:\n          path: '**/build/**/*.html'\n"); err != nil {
		t.Fatalf("bounded upload-artifact glob rejected: %v", err)
	}
	if _, err := compile(actionintegration.UploadArtifactV7Commit, "        with:\n          path: payload/result.txt\n          archive: false\n"); err == nil || !strings.Contains(err.Error(), `input "archive" may only be omitted or true`) {
		t.Fatalf("upload-artifact v7 raw mode error = %v", err)
	}
	for version, commit := range map[string]string{
		"v4": actionintegration.UploadArtifactCommit,
		"v5": actionintegration.UploadArtifactV5Commit,
		"v6": actionintegration.UploadArtifactV6Commit,
	} {
		if _, err := compile(commit, "        with:\n          path: payload/result.txt\n          archive: true\n"); err == nil || !strings.Contains(err.Error(), "only in actions/upload-artifact v7") {
			t.Fatalf("upload-artifact %s archive input error = %v", version, err)
		}
	}
	if _, err := compile(actionintegration.UploadArtifactCommit, "        with:\n          path: tests/*.log\n          retention-days: '7'\n"); err != nil {
		t.Fatalf("bounded glob and advisory retention rejected: %v", err)
	}

	for name, input := range map[string]string{
		"missing path":  "          name: payload\n",
		"bad retention": "          path: payload\n          retention-days: '-1'\n",
		"overwrite":     "          path: payload\n          overwrite: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compile(actionintegration.UploadArtifactCommit, "        with:\n"+input)
			if err == nil || !strings.Contains(err.Error(), "artifact.yml:") || !strings.Contains(err.Error(), "bounded upload-artifact adapter") {
				t.Fatalf("compilePlansForTest() error = %v", err)
			}
		})
	}

	if _, err := compile(strings.Repeat("b", 40), "        with:\n          path: payload\n"); err == nil || !strings.Contains(err.Error(), actionintegration.UploadArtifactCommit) || !strings.Contains(err.Error(), actionintegration.UploadArtifactV5Commit) || !strings.Contains(err.Error(), actionintegration.UploadArtifactV6Commit) || !strings.Contains(err.Error(), actionintegration.UploadArtifactV7Commit) {
		t.Fatalf("unsupported upload-artifact commit error = %v", err)
	}

	matrixWorkflow := []byte("on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    strategy:\n      matrix:\n        mode: [production, test]\n    steps:\n      - uses: actions/upload-artifact@" + actionintegration.UploadArtifactV6Commit + "\n        if: matrix.mode == 'test'\n        with:\n          name: ${{ github.sha }}\n          path: ./artifacts.tar.gz\n          retention-days: '0'\n")
	if err := os.WriteFile(workflowPath, matrixWorkflow, 0o644); err != nil {
		t.Fatal(err)
	}
	plans, err = compilePlansForTest(context.Background(), workflowPath, matrixWorkflow, pushEvent(t), "artifact-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatalf("conditional v6 matrix upload rejected: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("conditional v6 matrix produced %d plans, want 2", len(plans))
	}
	for _, job := range plans {
		if job.Steps[0].Condition != "matrix.mode == 'test'" || job.Steps[0].With["name"] != "${{ github.sha }}" || job.Steps[0].With["path"] != "./artifacts.tar.gz" || job.Actions[0].Commit != actionintegration.UploadArtifactV6Commit {
			t.Fatalf("conditional v6 matrix plan = %#v", job)
		}
	}
}

func TestDownloadArtifactAdapterInputCommitAndNeedsBoundary(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "artifact.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, remote, "", "name: artifact action\nruns:\n  using: node20\n  main: index.js\n")
	compile := func(commit, needs, with string) ([]plan.Job, error) {
		workflow := []byte("on: push\njobs:\n  producer:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/upload-artifact@" + actionintegration.UploadArtifactCommit + "\n        with:\n          path: payload\n  consumer:\n" + needs + "    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/download-artifact@" + commit + "\n" + with)
		if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
			t.Fatal(err)
		}
		return compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "artifact-test", testDistributionDigest, Options{
			EventTrust: EventUntrusted,
			Runners: RunnerPolicy{
				Labels:          map[string]string{"ubuntu-latest": "hosted"},
				UntrustedQueues: []string{"hosted"},
			},
			ResolveActions: true,
			ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
		})
	}

	for _, commit := range actionintegration.DownloadArtifactCommits() {
		with := "        with:\n          name: payload\n          path: out\n          merge-multiple: false\n"
		if commit == actionintegration.DownloadArtifactV8Commit || commit == actionintegration.DownloadArtifactV801Commit {
			with += "          skip-decompress: false\n          digest-mismatch: error\n"
		}
		plans, err := compile(commit, "    needs: producer\n", with)
		if err != nil {
			t.Fatalf("audited download-artifact commit %s: %v", commit, err)
		}
		if len(plans) != 2 || len(plans[1].NeedSources["producer"]) != 1 || plans[1].Actions[0].Commit != commit {
			t.Fatalf("download-artifact plans for %s = %#v", commit, plans)
		}
	}
	plans, err := compile(actionintegration.DownloadArtifactV5Commit, "    needs: producer\n", "        with:\n          pattern: 'junit-xml-25-*'\n          path: out\n          merge-multiple: true\n")
	if err != nil || len(plans) != 2 || plans[1].Actions[0].Commit != actionintegration.DownloadArtifactV5Commit {
		t.Fatalf("download-artifact v5 pattern plans = %#v, %v", plans, err)
	}
	plans, err = compile(actionintegration.DownloadArtifactV5Commit, "    needs: producer\n", "        with:\n          pattern: '{junit-results-backend,product-junit-results}-*'\n          path: out\n          merge-multiple: true\n")
	if err != nil || len(plans) != 2 || plans[1].Steps[0].With["pattern"] != "{junit-results-backend,product-junit-results}-*" {
		t.Fatalf("download-artifact PostHog pattern plans = %#v, %v", plans, err)
	}

	for name, with := range map[string]string{
		"missing name":  "        with:\n          path: out\n",
		"pattern":       "        with:\n          name: payload\n          pattern: 'payload-*'\n",
		"artifact IDs":  "        with:\n          name: payload\n          artifact-ids: '1'\n",
		"merge":         "        with:\n          name: payload\n          merge-multiple: true\n",
		"absolute path": "        with:\n          name: payload\n          path: /tmp/out\n",
		"drive path":    "        with:\n          name: payload\n          path: C:/out\n",
		"cross run":     "        with:\n          name: payload\n          run-id: '1'\n",
		"cross repo":    "        with:\n          name: payload\n          repository: owner/repo\n",
		"REST token":    "        with:\n          name: payload\n          github-token: token\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := compile(actionintegration.DownloadArtifactCommit, "    needs: producer\n", with)
			if err == nil || !strings.Contains(err.Error(), "bounded download-artifact adapter") {
				t.Fatalf("compilePlansForTest() error = %v", err)
			}
		})
	}
	if _, err := compile(actionintegration.DownloadArtifactCommit, "", "        with:\n          name: payload\n"); err == nil || !strings.Contains(err.Error(), "direct needs producer") {
		t.Fatalf("needs-free download-artifact error = %v", err)
	}
	if _, err := compile(strings.Repeat("b", 40), "    needs: producer\n", "        with:\n          name: payload\n"); err == nil || !strings.Contains(err.Error(), actionintegration.DownloadArtifactCommit) || !strings.Contains(err.Error(), actionintegration.DownloadArtifactV5Commit) {
		t.Fatalf("unsupported download-artifact commit error = %v", err)
	}
	for name, with := range map[string]string{
		"raw":             "        with:\n          name: payload\n          skip-decompress: true\n",
		"digest override": "        with:\n          name: payload\n          digest-mismatch: warn\n",
	} {
		t.Run("v8 "+name, func(t *testing.T) {
			if _, err := compile(actionintegration.DownloadArtifactV801Commit, "    needs: producer\n", with); err == nil || !strings.Contains(err.Error(), "bounded download-artifact adapter") {
				t.Fatalf("compilePlansForTest() error = %v", err)
			}
		})
	}
}

func TestDownloadArtifactCompilesConditionalProducerAndConsumerMatrixFanout(t *testing.T) {
	workspace, remote := t.TempDir(), t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "artifacts.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAction(t, remote, "", "name: artifact action\nruns:\n  using: node24\n  main: index.js\n")
	workflow := []byte(`on: push
jobs:
  producer:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        publish: [true, false]
    steps:
      - if: matrix.publish
        uses: actions/upload-artifact@` + actionintegration.UploadArtifactCommit + `
        with:
          name: ${{ github.sha }}
          path: payload
  consumer:
    needs: producer
    runs-on: ubuntu-latest
    strategy:
      matrix:
        shard: [one, two]
    steps:
      - uses: actions/download-artifact@` + actionintegration.DownloadArtifactV7Commit + `
        with:
          name: ${{ github.sha }}
          path: './'
`)
	if err := os.WriteFile(workflowPath, workflow, 0o644); err != nil {
		t.Fatal(err)
	}
	plans, err := compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "artifact-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 4 {
		t.Fatalf("plans = %d, want two producers and two consumers", len(plans))
	}
	for i, producer := range plans[:2] {
		if producer.Workflow.LogicalJobID != "producer" || len(producer.Steps) != 1 || producer.Steps[0].Condition != "matrix.publish" || producer.Matrix["publish"] != (i == 0) {
			t.Fatalf("producer %d = %#v", i, producer)
		}
	}
	for i, consumer := range plans[2:] {
		if consumer.Workflow.LogicalJobID != "consumer" || consumer.Matrix["shard"] != []string{"one", "two"}[i] || len(consumer.NeedSources["producer"]) != 2 {
			t.Fatalf("consumer %d fan-in = %#v", i, consumer)
		}
		if len(consumer.Steps) != 1 || consumer.Steps[0].With["name"] != "${{ github.sha }}" || consumer.Steps[0].With["path"] != "./" || len(consumer.Actions) != 1 || consumer.Actions[0].Commit != actionintegration.DownloadArtifactV7Commit {
			t.Fatalf("consumer %d download = %#v", i, consumer)
		}
	}
}

func TestDownloadArtifactMutableTagMustResolveToAuditedExactLock(t *testing.T) {
	remote := t.TempDir()
	writeAction(t, remote, "", "name: artifact action\nruns:\n  using: node24\n  main: index.js\n")
	resolved := &fakeActionSource{root: remote, calls: map[string]int{}, commit: actionintegration.DownloadArtifactV801Commit}
	_, locks, _, _, err := compileActionLocks(context.Background(), t.TempDir(), resolved, []string{"actions/download-artifact@v8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || locks[0].RequestedRef != "v8" || locks[0].Commit != actionintegration.DownloadArtifactV801Commit {
		t.Fatalf("mutable v8 lock = %#v", locks)
	}

	resolved.commit = strings.Repeat("b", 40)
	if _, _, _, _, err := compileActionLocks(context.Background(), t.TempDir(), resolved, []string{"actions/download-artifact@v8"}); err == nil {
		t.Fatal("mutable v8 tag resolving to an unaudited commit was accepted")
	}
}

func TestCompilePlansDockerfileActionCapabilities(t *testing.T) {
	remote := t.TempDir()
	writeAction(t, remote, "", "name: remote Docker\nruns:\n  using: docker\n  image: Dockerfile\n")
	workflowPath := filepath.Join("..", "..", "testdata", "dockerfile-action", ".github", "workflows", "docker-action.yml")
	templatePath := workflowPath + ".tmpl"
	plans, err := compilePlansForTest(context.Background(), workflowPath, readFile(t, templatePath), pushEvent(t), "dockerfile-action-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   &fakeActionSource{root: remote, calls: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !reflect.DeepEqual(plans[0].RequiredCapabilities, []string{"docker", "network"}) || len(plans[0].Actions) != 1 || plans[0].Actions[0].Source != "github" {
		t.Fatalf("Dockerfile action plans = %#v", plans)
	}
}

func TestCompilePlansRemoteActionRequiresSource(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "remote.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/repo@v1\n")
	_, err := compilePlansForTest(context.Background(), workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
	})
	if err == nil || !strings.Contains(err.Error(), "remote action source is not configured") {
		t.Fatalf("compilePlansForTest() error = %v, want source configuration rejection", err)
	}
}

func TestCompilePlansContextCancelsRemoteResolution(t *testing.T) {
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "remote.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte("on: push\njobs:\n  action:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: owner/repo@v1\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := compilePlansForTest(ctx, workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   contextActionSource{},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("compilePlansForTest() error = %v, want context cancellation", err)
	}
}

func TestLivePublicActionCompatibility(t *testing.T) {
	if os.Getenv("BUILDKITE_GHA_LIVE_ACTIONS") != "1" {
		t.Skip("set BUILDKITE_GHA_LIVE_ACTIONS=1 to query and download public GitHub actions anonymously")
	}
	workspace := t.TempDir()
	workflowPath := filepath.Join(workspace, ".github", "workflows", "public-actions.yml")
	if err := os.MkdirAll(filepath.Dir(workflowPath), 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := []byte(`on: push
jobs:
  actions:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - uses: actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38
        with:
          node-version: "24"
          package-manager-cache: "false"
      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16
        with:
          go-version: "1.26.5"
          cache: "false"
`)
	resolver, err := source.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	actionCache := filepath.Join(t.TempDir(), "actions")
	if err := os.Mkdir(actionCache, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := source.NewStore(actionCache, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	plans, err := compilePlansForTest(ctx, workflowPath, workflow, pushEvent(t), "0.0.0-test", testDistributionDigest, Options{
		EventTrust: EventUntrusted,
		Runners: RunnerPolicy{
			Labels:          map[string]string{"ubuntu-latest": "hosted"},
			UntrustedQueues: []string{"hosted"},
		},
		ResolveActions: true,
		ActionSource:   PublicActionSource{Resolver: resolver, Store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Schema != plan.Schema || len(plans[0].Actions) != 3 || plans[0].RequiresMise == nil || !*plans[0].RequiresMise {
		t.Fatalf("public action plan = %#v", plans)
	}
	wantCommits := map[string]string{
		"actions/checkout":   "3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/setup-node": "249970729cb0ef3589644e2896645e5dc5ba9c38",
		"actions/setup-go":   "924ae3a1cded613372ab5595356fb5720e22ba16",
	}
	for _, lock := range plans[0].Actions {
		if want := wantCommits[lock.Repository]; lock.Commit != want || lock.SourceDigest == "" {
			t.Fatalf("public action lock = %#v, want commit %q", lock, want)
		}
	}
	checkoutRoot := filepath.Join(actionCache, "actions", "checkout", wantCommits["actions/checkout"], "tree")
	checkout, err := metadata.Load(checkoutRoot, ".")
	if err != nil {
		t.Fatal(err)
	}
	if token := checkout.Inputs["token"].Default; token == nil || *token != "${{ github.token }}" {
		t.Fatalf("actions/checkout token default = %#v, want github.token", token)
	}
	distribution, err := os.ReadFile(filepath.Join(checkoutRoot, "dist", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(distribution, []byte("getInput('token', { required: true })")) || !bytes.Contains(distribution, []byte("Input required and not supplied:")) {
		t.Fatal("pinned actions/checkout no longer requires a token before Git; revisit the built-in tokenless adapter")
	}
}
