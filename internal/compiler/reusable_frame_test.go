package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/buildkite/buildkite-gha/internal/workflow"
)

func testRootFrame(t *testing.T, parsed *workflow.Workflow) callFrame {
	t.Helper()
	source, err := rootWorkflowSource(filepath.Join(t.TempDir(), ".github", "workflows", "root.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return rootFrame(source, "sha256:root", parsed, map[string]any{"name": "root"})
}

func TestRootFrameInheritsNothing(t *testing.T) {
	parsed := &workflow.Workflow{Permissions: &workflow.Permissions{Scopes: map[string]string{"contents": "write"}}}
	frame := testRootFrame(t, parsed)

	if !frame.isRoot() || frame.depth != 0 {
		t.Fatalf("root frame depth = %d, want 0", frame.depth)
	}
	if frame.permissionCeiling != nil {
		t.Fatalf("root permission ceiling = %#v, want nil", frame.permissionCeiling)
	}
	if !frame.secrets.unrestricted || len(frame.secrets.bindings) != 0 {
		t.Fatalf("root secret authority = %#v, want unrestricted", frame.secrets)
	}
	if frame.namespace != "" || frame.label != "" || frame.needs != nil || frame.guards != nil || frame.gates != nil || frame.tokenPolicyNarrowed || frame.callPosition != (workflow.Position{}) {
		t.Fatalf("root frame inherited caller state: %#v", frame)
	}
	if frame.path() != "./.github/workflows/root.yml" || !reflect.DeepEqual(frame.chain, []reusableSourceIdentity{frame.source.identity}) {
		t.Fatalf("root frame source = %q chain %#v", frame.path(), frame.chain)
	}
	if !reflect.DeepEqual(frame.inputs, reusableInputs{values: map[string]any{"name": "root"}}) {
		t.Fatalf("root inputs = %#v", frame.inputs)
	}
}

func TestCallFrameChildDerivesCalleeState(t *testing.T) {
	root := testRootFrame(t, &workflow.Workflow{})
	root.gates = []WorkflowConcurrencyGate{{ID: "outer", Group: "group"}}
	callSpan := workflow.Span{Start: workflow.Position{Line: 4, Column: 5}, End: workflow.Position{Line: 4, Column: 40}}
	callee := reusableWorkflowSource{identity: reusableSourceIdentity{kind: "workspace", repository: root.source.repositoryRoot, path: ".github/workflows/callee.yml"}, repositoryRoot: root.source.repositoryRoot, displayPath: "./.github/workflows/callee.yml"}
	site := callSite{
		job:                 workflow.Job{ID: "call", Name: "Call ${{ matrix.os }}"},
		call:                &workflow.ReusableWorkflowCall{Uses: "./.github/workflows/callee.yml", Span: callSpan},
		needs:               map[string]needBinding{"setup": {members: []string{"setup"}}},
		guards:              []sourcedCallGuard{{condition: "true"}},
		tokenPolicyNarrowed: true,
		callee:              callee, calleeDigest: "sha256:callee", calleeWorkflow: &workflow.Workflow{Callable: true},
		inputs:  reusableInputs{values: map[string]any{"name": "callee"}},
		secrets: secretAuthority{bindings: map[string]secretBinding{"token": {source: "TOKEN"}}},
	}

	child, err := root.child(site)
	if err != nil {
		t.Fatal(err)
	}
	if child.isRoot() || child.depth != 1 {
		t.Fatalf("child depth = %d, want 1", child.depth)
	}
	if child.namespace != "call" || child.label != "Call ${{ matrix.os }}" {
		t.Fatalf("child namespace = %q label %q", child.namespace, child.label)
	}
	if !reflect.DeepEqual(child.permissionCeiling, &workflow.Permissions{Scopes: map[string]string{}, Span: callSpan}) {
		t.Fatalf("child ceiling without caller permissions = %#v, want empty map at the call", child.permissionCeiling)
	}
	if child.callPosition != callSpan.Start {
		t.Fatalf("child call position = %#v, want outermost call %#v", child.callPosition, callSpan.Start)
	}
	if child.path() != callee.displayPath || child.digest != "sha256:callee" || child.workflow != site.calleeWorkflow {
		t.Fatalf("child source = %q digest %q", child.path(), child.digest)
	}
	if !reflect.DeepEqual(child.chain, []reusableSourceIdentity{root.source.identity, callee.identity}) {
		t.Fatalf("child chain = %#v", child.chain)
	}
	if !reflect.DeepEqual(child.needs, site.needs) || !reflect.DeepEqual(child.guards, site.guards) || !child.tokenPolicyNarrowed {
		t.Fatalf("child did not forward call site state: %#v", child)
	}
	if !reflect.DeepEqual(child.inputs, site.inputs) || !reflect.DeepEqual(child.secrets, site.secrets) || child.secrets.unrestricted {
		t.Fatalf("child inputs = %#v secrets %#v", child.inputs, child.secrets)
	}
	if !reflect.DeepEqual(child.gates, root.gates) {
		t.Fatalf("child gates = %#v, want inherited %#v", child.gates, root.gates)
	}
	child.gates = append(child.gates, WorkflowConcurrencyGate{ID: "call", Group: "inner"})
	if len(root.gates) != 1 {
		t.Fatalf("appending child gates mutated the caller: %#v", root.gates)
	}

	// A nested call keeps the outermost call position, narrows to the caller
	// job's permissions, and extends namespace and label with the matrix.
	site.job.Permissions = &workflow.Permissions{Scopes: map[string]string{"contents": "read"}}
	site.matrix = map[string]any{"os": "linux"}
	site.instances = 2
	grandchild, err := child.child(site)
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := matrixDigest(site.matrix)
	if err != nil {
		t.Fatal(err)
	}
	if grandchild.depth != 2 || grandchild.namespace != "call.call-"+suffix || grandchild.label != "Call ${{ matrix.os }} / Call linux" {
		t.Fatalf("grandchild namespace = %q label %q depth %d", grandchild.namespace, grandchild.label, grandchild.depth)
	}
	if grandchild.callPosition != callSpan.Start || grandchild.permissionCeiling != site.job.Permissions {
		t.Fatalf("grandchild call position = %#v ceiling %#v", grandchild.callPosition, grandchild.permissionCeiling)
	}
	if cycle := grandchild.cycle(callee); cycle != ".github/workflows/callee.yml -> .github/workflows/callee.yml -> .github/workflows/callee.yml" {
		t.Fatalf("cycle = %q", cycle)
	}
	if cycle := grandchild.cycle(reusableWorkflowSource{identity: reusableSourceIdentity{kind: "workspace", path: "other.yml"}}); cycle != "" {
		t.Fatalf("cycle for unseen source = %q, want none", cycle)
	}
}

func TestCallFrameJobPermissions(t *testing.T) {
	rootPermissions := &workflow.Permissions{Scopes: map[string]string{"contents": "read", "issues": "write"}}
	root := testRootFrame(t, &workflow.Workflow{Permissions: rootPermissions})
	declared := &workflow.Permissions{Scopes: map[string]string{"contents": "write", "id-token": "write"}}

	got := root.jobPermissions(declared, rootPermissions)
	if !reflect.DeepEqual(got.permissions.Scopes, declared.Scopes) || got.tokenPolicyNarrowed {
		t.Fatalf("root job permissions = %#v, want declared map unclamped", got)
	}
	if !got.jobPermissionsIgnored {
		t.Fatal("root job permissions differing from the workflow must be flagged for the call warning")
	}
	got = root.jobPermissions(nil, rootPermissions)
	if !reflect.DeepEqual(got.permissions.Scopes, rootPermissions.Scopes) || got.jobPermissionsIgnored || got.tokenPolicyNarrowed {
		t.Fatalf("root job default permissions = %#v, want workflow permissions", got)
	}

	child, err := root.child(callSite{
		job:            workflow.Job{ID: "call", Permissions: &workflow.Permissions{Scopes: map[string]string{"contents": "read", "id-token": "write"}}},
		call:           &workflow.ReusableWorkflowCall{Uses: "./.github/workflows/callee.yml"},
		callee:         reusableWorkflowSource{identity: reusableSourceIdentity{kind: "workspace", path: ".github/workflows/callee.yml"}, displayPath: "./.github/workflows/callee.yml"},
		calleeWorkflow: &workflow.Workflow{Callable: true},
		secrets:        secretAuthority{},
	})
	if err != nil {
		t.Fatal(err)
	}
	got = child.jobPermissions(declared, rootPermissions)
	if !reflect.DeepEqual(got.permissions.Scopes, map[string]string{"contents": "read", "issues": "write", "id-token": "write"}) {
		t.Fatalf("callee job permissions = %#v, want root repository permissions plus bounded id-token", got.permissions.Scopes)
	}
	if !got.tokenPolicyNarrowed || !got.jobPermissionsIgnored {
		t.Fatalf("callee job flags = %#v, want narrowed and ignored", got)
	}
	got = child.jobPermissions(nil, rootPermissions)
	if !reflect.DeepEqual(got.permissions.Scopes, map[string]string{"contents": "read", "issues": "write", "id-token": "write"}) || got.jobPermissionsIgnored {
		t.Fatalf("callee default permissions = %#v, want caller ceiling", got)
	}
}

func TestCallFrameJobNeeds(t *testing.T) {
	root := testRootFrame(t, &workflow.Workflow{})
	root.needs = map[string]needBinding{"setup": {members: []string{"setup"}, outputs: []needOutputBinding{{}}}}
	replacements := map[string]needBinding{"build": {members: []string{"call.build"}, projectOutputs: true}}

	own, effective := root.jobNeeds(workflow.Job{ID: "test", Needs: []string{"build"}}, replacements)
	if !reflect.DeepEqual(own, map[string]needBinding{"build": replacements["build"]}) || !reflect.DeepEqual(effective, own) {
		t.Fatalf("declared needs = %#v / %#v", own, effective)
	}

	own, effective = root.jobNeeds(workflow.Job{ID: "test"}, replacements)
	if len(own) != 0 {
		t.Fatalf("own needs without declarations = %#v", own)
	}
	if !reflect.DeepEqual(effective, map[string]needBinding{"setup": {members: []string{"setup"}, projectOutputs: true}}) {
		t.Fatalf("inherited needs = %#v, want caller needs without outputs", effective)
	}
	if root.needs["setup"].outputs == nil {
		t.Fatal("inheriting needs mutated the frame")
	}
}

func TestCompileDirectWorkflowOutsideRepositoryWorkflows(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "some", "dir", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(`on: push
permissions:
  contents: read
  issues: write
jobs:
  b:
    needs: a
    runs-on: ubuntu-latest
    env:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      DEPLOY: ${{ secrets.DEPLOY_TOKEN }}
    steps: [{run: true}]
  a:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := CompileBundle(path, source, pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 2 || bundle.IR.Jobs[0].LogicalJobID != "a" || bundle.IR.Jobs[1].LogicalJobID != "b" || bundle.IR.Jobs[0].SourcePath != path {
		t.Fatalf("direct workflow jobs = %#v", bundle.IR.Jobs)
	}
	job := bundle.Plans[1].Job
	if job.Workflow.Remote != nil || job.GitHubToken == nil || !reflect.DeepEqual(job.GitHubToken.Permissions, map[string]string{"contents": "read", "issues": "write"}) {
		t.Fatalf("direct workflow token = %#v remote %#v", job.GitHubToken, job.Workflow.Remote)
	}
	if !reflect.DeepEqual(job.RequiredSecrets, []string{"DEPLOY_TOKEN"}) || !reflect.DeepEqual(job.RequiredCapabilities, []string{"provider-token-write", "secrets"}) {
		t.Fatalf("direct workflow secrets = %#v capabilities %#v", job.RequiredSecrets, job.RequiredCapabilities)
	}
	if len(bundle.IR.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", bundle.IR.Warnings)
	}
}

func TestCompileRejectsReusableCallOutsideRepositoryWorkflows(t *testing.T) {
	source := []byte(`on: push
jobs:
  delegated:
    uses: ./.github/workflows/reusable.yml
`)
	_, err := Compile("ci.yml", source, pushEvent(t))
	if err == nil || !strings.Contains(err.Error(), `ci.yml:4:11: job "delegated": reusable workflows require the caller under .github/workflows`) {
		t.Fatalf("Compile() error = %v, want located caller location rejection", err)
	}
}

func TestCompileRejectsDirectWorkflowNeedsCycle(t *testing.T) {
	source := []byte(`on: push
jobs:
  a:
    needs: b
    runs-on: ubuntu-latest
    steps: [{run: true}]
  b:
    needs: a
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	_, err := Compile(".github/workflows/cycle.yml", source, pushEvent(t))
	if err == nil || err.Error() != `./.github/workflows/cycle.yml:3:3: job "a": workflow job graph contains a cycle` {
		t.Fatalf("Compile() error = %v, want cycle rejection", err)
	}
	report, err := Validate(".github/workflows/cycle.yml", source)
	if err == nil || report.Instances != 0 || len(report.Jobs) != 0 {
		t.Fatalf("Validate() = %#v, %v, want no evaluated jobs", report, err)
	}
}

func TestCompileBoundsDirectWorkflowGraphExpansion(t *testing.T) {
	var source strings.Builder
	source.WriteString("on: push\njobs:\n")
	for i := range maxFlattenedJobs + 1 {
		_, _ = fmt.Fprintf(&source, "  job%d:\n    runs-on: ubuntu-latest\n    steps: [{run: true}]\n", i)
	}
	_, err := Compile(".github/workflows/large.yml", []byte(source.String()), pushEvent(t))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("workflow job graph expands beyond %d jobs", maxFlattenedJobs)) {
		t.Fatalf("Compile() error = %v, want bounded graph rejection", err)
	}
}

func TestCompileCallableRootWorkflowKeepsRuntimeInputExpressions(t *testing.T) {
	repository := t.TempDir()
	caller := writeWorkflow(t, repository, "caller.yml", `on:
  push:
  workflow_call:
    inputs:
      package:
        type: string
jobs:
  build:
    runs-on: ubuntu-latest
    env:
      PACKAGE: ${{ inputs.package }}
    steps: [{run: true}]
  delegated:
    uses: ./.github/workflows/reusable.yml
`)
	writeWorkflow(t, repository, "reusable.yml", `on: workflow_call
jobs:
  test:
    runs-on: ubuntu-latest
    steps: [{run: true}]
`)
	bundle, err := CompileBundle(caller, readFile(t, caller), pushEvent(t), "0.0.0-test", testDistributionDigest, "gha-importer")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plans) != 2 || bundle.IR.Jobs[0].LogicalJobID != "build" || bundle.IR.Jobs[1].LogicalJobID != "delegated.test" {
		t.Fatalf("callable root jobs = %#v", bundle.IR.Jobs)
	}
}
