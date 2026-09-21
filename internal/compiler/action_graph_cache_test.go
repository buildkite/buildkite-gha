package compiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestActionGraphCacheReusesGraphButNotAuthorityOrResults(t *testing.T) {
	workspace := t.TempDir()
	writeAction(t, workspace, "child", "name: child\ninputs:\n  token:\n    default: ${{ github.token }}\nruns:\n  using: node24\n  main: index.js\n")
	writeAction(t, workspace, "parent", "name: parent\ninputs:\n  token:\n    default: ''\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n      with:\n        token: ${{ inputs.token }}\n")
	instance := JobInstance{RepositoryRoot: workspace, SourcePath: "ci.yml"}
	refs := []string{"./parent", "./child"}
	cache := newActionGraphCache(Options{})
	inputs := []map[string]string{{"token": ""}, {"token": ""}}
	want, err := compileActionInvocations(t.Context(), workspace, nil, "https://github.com", refs, inputs)
	if err != nil {
		t.Fatal(err)
	}
	first, err := cache.compile(t.Context(), instance, "https://github.com", refs, inputs)
	if err != nil {
		t.Fatal(err)
	}
	programJSON := func(compiled actionCompilation) string {
		t.Helper()
		encoded, err := json.Marshal(compiled.programs)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	// Cloning normalizes empty slices to nil; their serialized plans agree.
	if programJSON(first) != programJSON(want) || !reflect.DeepEqual(first.selectors, want.selectors) || !reflect.DeepEqual(first.rootAuthorities, want.rootAuthorities) {
		t.Fatal("cached compilation differs from uncached compilation")
	}
	// No metadata or entrypoint remains: later successes require graph reuse.
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	first.capabilities = append(first.capabilities, "corrupted")
	first.locks[0].SourceDigest = "corrupted"
	for _, lock := range first.locks {
		clear(lock.Children)
	}
	for _, action := range first.programs {
		if len(action.Inputs) > 0 && action.Inputs[0].Default != nil {
			action.Inputs[0].Default.Source = "corrupted"
		}
	}
	clear(first.programs)
	for _, test := range []struct {
		inputs []map[string]string
		server string
		want   bool
	}{
		{[]map[string]string{{"token": "${{ github.token }}"}, {"token": ""}}, "https://github.com", true},
		{[]map[string]string{{"token": ""}, {}}, "https://github.com", true},
		{[]map[string]string{{"token": "${{ github.server_url == 'https://github.com' && github.token || '' }}"}, {"token": ""}}, "https://other.example", false},
		{nil, "https://github.com", false},
		{inputs, "https://github.com", false},
	} {
		got, err := cache.compile(t.Context(), instance, test.server, refs, test.inputs)
		if err != nil {
			t.Fatal(err)
		}
		if got.requiresGitHubToken != test.want {
			t.Fatalf("inputs %v on %s: token = %v, want %v", test.inputs, test.server, got.requiresGitHubToken, test.want)
		}
		if programJSON(got) != programJSON(want) || !reflect.DeepEqual(got.locks, want.locks) || !reflect.DeepEqual(got.capabilities, want.capabilities) {
			t.Fatal("mutating a prior result corrupted the cached graph")
		}
	}
	if _, err := newActionGraphCache(Options{}).compile(t.Context(), instance, "https://github.com", refs, inputs); err == nil {
		t.Fatal("a new bundle reused another bundle's graph")
	}
}

func TestActionGraphCacheSeparatesSourceAndOrderedRoots(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"one", "two"} {
		writeAction(t, workspace, name, "name: action\nruns:\n  using: node24\n  main: index.js\n")
	}
	instance := JobInstance{RepositoryRoot: workspace, SourcePath: "ci.yml", SourceDigest: "original", RemoteWorkflow: &RemoteWorkflowSource{Repository: "owner/repo", RequestedRef: "main", Commit: "original", SourceDigest: "original"}}
	refs := []string{"./one", "./two"}
	cache := newActionGraphCache(Options{})
	if _, err := cache.compile(t.Context(), instance, "https://github.com", refs, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*JobInstance)
		refs   []string
	}{
		{name: "workspace", change: func(i *JobInstance) { i.RepositoryRoot = filepath.Join(workspace, "other") }},
		{name: "workflow path", change: func(i *JobInstance) { i.SourcePath = "other.yml" }},
		{name: "workflow digest", change: func(i *JobInstance) { i.SourceDigest = "other" }},
		{name: "local workflow", change: func(i *JobInstance) { i.RemoteWorkflow = nil }},
		{name: "repository", change: func(i *JobInstance) { i.RemoteWorkflow.Repository = "owner/other" }},
		{name: "ref", change: func(i *JobInstance) { i.RemoteWorkflow.RequestedRef = "other" }},
		{name: "commit", change: func(i *JobInstance) { i.RemoteWorkflow.Commit = "other" }},
		{name: "tree", change: func(i *JobInstance) { i.RemoteWorkflow.SourceDigest = "other" }},
		{name: "order", refs: []string{"./two", "./one"}},
		{name: "duplicate", refs: []string{"./one", "./two", "./one"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := instance
			remote := *instance.RemoteWorkflow
			changed.RemoteWorkflow = &remote
			if test.change != nil {
				test.change(&changed)
			}
			requested := refs
			if test.refs != nil {
				requested = test.refs
			}
			if _, err := cache.compile(t.Context(), changed, "https://github.com", requested, nil); err == nil {
				t.Fatal("different source or roots reused the cached graph")
			}
		})
	}
}

func TestActionGraphCacheRetriesFailedConstruction(t *testing.T) {
	workspace := t.TempDir()
	writeAction(t, workspace, "parent", "name: parent\nruns:\n  using: composite\n  steps:\n    - uses: ./child\n")
	instance := JobInstance{RepositoryRoot: workspace}
	cache := newActionGraphCache(Options{})
	refs := []string{"./parent"}
	if _, err := cache.compile(t.Context(), instance, "https://github.com", refs, nil); err == nil {
		t.Fatal("missing child was accepted")
	}
	writeAction(t, workspace, "child", "name: child\nruns:\n  using: node24\n  main: index.js\n")
	got, err := cache.compile(t.Context(), instance, "https://github.com", refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.locks) != 2 || len(got.programs) != 2 {
		t.Fatal("retry reused a partial graph")
	}
}
