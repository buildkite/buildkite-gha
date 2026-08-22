package program

import "testing"

func TestInventoryActionAuthorityNarrowsActionDefaultsByProvider(t *testing.T) {
	value := testActionSite("${{ github.server_url == 'https://github.com' && github.token || '' }}")
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeJavaScript, Inputs: []ActionInput{{Name: "token", Default: &value}}, JavaScript: &JavaScriptAction{Main: "index.js"}},
	}
	for _, test := range []struct {
		name      string
		serverURL string
		wantToken bool
	}{
		{name: "GitHub", serverURL: "https://github.com", wantToken: true},
		{name: "Origin", serverURL: "https://origin.cursor.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{ServerURL: test.serverURL})
			if err != nil {
				t.Fatal(err)
			}
			if authority.GitHubToken != test.wantToken {
				t.Fatalf("GitHub token authority = %v, want %v", authority.GitHubToken, test.wantToken)
			}
		})
	}
}

func TestInventoryActionAuthorityFollowsCompositeReachability(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source:  "workspace",
			Runtime: ActionRuntimeComposite,
			Inputs:  []ActionInput{{Name: "enabled"}},
			Composite: &CompositeAction{Steps: []CompositeStep{{
				Condition: testActionSite("inputs.enabled == 'true'"),
				Run:       &Run{Command: testActionSite("echo ${{ github.token }}")},
			}}},
		},
	}
	for _, test := range []struct {
		name      string
		input     string
		context   ActionAuthorityContext
		wantToken bool
	}{
		{name: "known false", input: "${{ false }}"},
		{name: "known true", input: "${{ true }}", wantToken: true},
		{name: "unknown", input: "${{ matrix.enabled }}", wantToken: true},
		{name: "known matrix false", input: "${{ matrix.enabled }}", context: ActionAuthorityContext{Matrix: map[string]any{"enabled": false}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root", Inputs: []Binding{{Name: "enabled", Value: testWorkflowSite(test.input)}}}, test.context)
			if err != nil {
				t.Fatal(err)
			}
			if authority.GitHubToken != test.wantToken {
				t.Fatalf("GitHub token authority = %v, want %v", authority.GitHubToken, test.wantToken)
			}
		})
	}
}

func TestInventoryActionAuthorityIncludesRemotePreWhenMainIsSkipped(t *testing.T) {
	value := testActionSite("${{ github.token }}")
	actions := map[string]Action{
		"root": {
			Source:     "github",
			Runtime:    ActionRuntimeJavaScript,
			Inputs:     []ActionInput{{Name: "token", Default: &value}},
			JavaScript: &JavaScriptAction{Pre: "pre.js", Main: "index.js"},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root", Condition: testWorkflowSite("false")}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("remote pre-if did not grant GitHub token authority when the main step was skipped")
	}
}

func TestInventoryActionAuthorityDoesNotGrantWholeGitHubContextFromCompositeMetadata(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source:    "workspace",
			Runtime:   ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{{Run: &Run{Command: testActionSite("echo ${{ toJSON(github) }}")}}}},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("composite-authored toJSON(github) granted GitHub token authority")
	}
}

func TestInventoryActionAuthorityPreparesOnlyFieldsUsedByChildLifecycle(t *testing.T) {
	for _, test := range []struct {
		name      string
		pre       string
		wantToken bool
	}{
		{name: "child without pre"},
		{name: "child with pre", pre: "pre.js", wantToken: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			actions := map[string]Action{
				"root": {
					Source:  "github",
					Runtime: ActionRuntimeComposite,
					Composite: &CompositeAction{Steps: []CompositeStep{{
						Env:        []Binding{{Name: "TOKEN", Value: testActionSite("${{ github.token }}")}},
						Invocation: &Invocation{Lock: "child", Uses: testActionSite("owner/child@v1")},
					}}},
				},
				"child": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{Pre: test.pre, Main: "index.js"}},
			}
			authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root", Condition: testWorkflowSite("false")}, ActionAuthorityContext{})
			if err != nil {
				t.Fatal(err)
			}
			if authority.GitHubToken != test.wantToken {
				t.Fatalf("GitHub token authority = %v, want %v", authority.GitHubToken, test.wantToken)
			}
		})
	}
}

func TestInventoryActionAuthorityForgetsEnvironmentAfterExecutableStep(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source:  "workspace",
			Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{
				{Run: &Run{Command: testActionSite("echo FLAG=true >> $GITHUB_ENV")}},
				{Condition: testActionSite("env.FLAG == 'true'"), Run: &Run{Command: testActionSite("echo ${{ github.token }}")}},
			}},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{Environment: map[string]string{"FLAG": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("a preceding executable step did not make the later environment guard conservative")
	}
}

func testActionSite(source string) Site {
	return Site{Source: source, Provenance: ProvenanceAction}
}

func testWorkflowSite(source string) Site {
	return Site{Source: source, Provenance: ProvenanceWorkflow}
}
