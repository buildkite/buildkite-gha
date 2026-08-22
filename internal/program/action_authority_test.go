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

func TestInventoryActionAuthorityIncludesInlineJavaScriptPre(t *testing.T) {
	for _, test := range []struct {
		name    string
		actions map[string]Action
	}{
		{
			name: "workspace action",
			actions: map[string]Action{
				"root": {Source: "workspace", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{Pre: "pre.js", PreCondition: testActionSite("github.token != ''"), Main: "index.js"}},
			},
		},
		{
			name: "remote child of workspace composite",
			actions: map[string]Action{
				"root":  {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{{Invocation: &Invocation{Lock: "child", Uses: testActionSite("owner/child@v1")}}}}},
				"child": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{Pre: "pre.js", PreCondition: testActionSite("github.token != ''"), Main: "index.js"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority, err := InventoryActionAuthority(test.actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
			if err != nil {
				t.Fatal(err)
			}
			if !authority.GitHubToken {
				t.Fatal("inline JavaScript pre-if did not grant GitHub token authority")
			}
		})
	}
}

func TestInventoryActionAuthorityRetainsConcreteMatrixAggregate(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "token"}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "token", Value: testWorkflowSite(`${{ contains(toJSON(matrix), '"publish": true') && github.token || '' }}`)}},
	}, ActionAuthorityContext{Matrix: map[string]any{"publish": false}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("known-false matrix aggregate retained GitHub token authority")
	}
}

func TestInventoryActionAuthorityUsesWorkflowInputsForLifecycleConditions(t *testing.T) {
	token := testActionSite("${{ github.token }}")
	actions := map[string]Action{
		"root": {
			Source: "github", Runtime: ActionRuntimeJavaScript, Inputs: []ActionInput{{Name: "publish"}, {Name: "token", Default: &token}},
			JavaScript: &JavaScriptAction{Pre: "pre.js", PreCondition: testActionSite("inputs.publish == 'true'"), Main: "index.js"},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root", Condition: testWorkflowSite("false"), Inputs: []Binding{{Name: "publish", Value: testWorkflowSite("false")}}}, ActionAuthorityContext{WorkflowInputs: map[string]any{"publish": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("workflow input used by pre-if did not grant GitHub token authority")
	}
}

func TestInventoryActionAuthorityIncludesRemoteCompositePreparationInputs(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "github", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "token"}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root", Condition: testWorkflowSite("false"), Inputs: []Binding{{Name: "token", Value: testWorkflowSite("${{ github.token }}")}}}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("remote composite preparation input did not grant GitHub token authority")
	}
}

func TestInventoryActionAuthorityTreatsOmittedOptionalInputAsEmpty(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "publish"}}, Composite: &CompositeAction{Steps: []CompositeStep{{Condition: testActionSite("inputs.publish == 'true'"), Run: &Run{Command: testActionSite("echo ${{ github.token }}")}}}}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("omitted optional input kept an unreachable token branch alive")
	}
}

func TestInventoryActionAuthorityTreatsOmittedOptionalInputAsNull(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "publish"}}, Composite: &CompositeAction{Steps: []CompositeStep{{
			Condition: testActionSite("toJSON(inputs.publish) == 'null'"), Run: &Run{Command: testActionSite("echo ${{ github.token }}")},
		}}}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("omitted optional input did not behave as null when referenced directly")
	}
}

func TestInventoryActionAuthorityKeepsOmittedOptionalInputOutOfAggregate(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "publish"}}, Composite: &CompositeAction{Steps: []CompositeStep{{
			Condition: testActionSite(`!contains(toJSON(inputs), '"publish"')`), Run: &Run{Command: testActionSite("echo ${{ github.token }}")},
		}}}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("omitted optional input appeared in aggregate inputs")
	}
}

func TestInventoryActionAuthorityUsesWorkflowInputsForNestedLifecycle(t *testing.T) {
	token := testActionSite("${{ github.token }}")
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "publish"}},
			Composite: &CompositeAction{Steps: []CompositeStep{{Invocation: &Invocation{Lock: "child", Uses: testActionSite("owner/child@v1")}}}},
		},
		"child": {
			Source: "github", Runtime: ActionRuntimeJavaScript, Inputs: []ActionInput{{Name: "token", Default: &token}},
			JavaScript: &JavaScriptAction{Pre: "pre.js", PreCondition: testActionSite("inputs.publish == 'true'"), Main: "index.js"},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "publish", Value: testWorkflowSite("false")}},
	}, ActionAuthorityContext{WorkflowInputs: map[string]any{"publish": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("nested lifecycle condition did not use workflow inputs")
	}
}

func TestInventoryActionAuthorityRetainsNestedMatrixReferences(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "token"}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "token", Value: testWorkflowSite("${{ matrix.config.publish && github.token || '' }}")}},
	}, ActionAuthorityContext{Matrix: map[string]any{"config": map[string]any{"publish": false}}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("known-false nested matrix value retained GitHub token authority")
	}
}

func TestInventoryActionAuthorityTreatsMissingConcreteMatrixMemberAsNull(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "token"}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "token", Value: testWorkflowSite("${{ matrix.missing && github.token || '' }}")}},
	}, ActionAuthorityContext{Matrix: map[string]any{"present": true}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("missing concrete matrix member retained GitHub token authority")
	}
}

func TestInventoryActionAuthorityRetainsWorkflowInputAggregate(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "token"}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "token", Value: testWorkflowSite("${{ inputs[matrix.key] && github.token || '' }}")}},
	}, ActionAuthorityContext{WorkflowInputs: map[string]any{"publish": false}, Matrix: map[string]any{"key": "publish"}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("known-false computed workflow input retained GitHub token authority")
	}
}

func TestInventoryActionAuthorityTreatsLaterAbsentInputAsEmptyInDefault(t *testing.T) {
	token := testActionSite("${{ inputs.enabled && github.token || '' }}")
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite,
			Inputs: []ActionInput{{Name: "token", Default: &token}, {Name: "enabled"}}, Composite: &CompositeAction{},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("later absent input kept a known-false default token branch reachable")
	}
}

func TestInventoryActionAuthorityTreatsLaterDefaultedInputAsNullDuringEarlierDefault(t *testing.T) {
	token := testActionSite("${{ toJSON(inputs.z_later) == 'null' && github.token || '' }}")
	later := testActionSite("later")
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite,
			Inputs: []ActionInput{{Name: "a_token", Default: &token}, {Name: "z_later", Default: &later}}, Composite: &CompositeAction{},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("later defaulted input did not behave as null while resolving an earlier default")
	}
}

func TestInventoryActionAuthorityKeepsRuntimeDependentDefaultErrorsConservative(t *testing.T) {
	defaultValue := testActionSite("${{ inputs.enabled && fromJSON('bad') && github.token || '' }}")
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "enabled"}, {Name: "token", Default: &defaultValue}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "enabled", Value: testWorkflowSite("${{ inputs.enabled }}")}},
	}, ActionAuthorityContext{UnknownWorkflowInputs: map[string]bool{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("runtime-dependent default error did not retain conservative token authority")
	}
}

func TestInventoryActionAuthorityRetainsStableInvocationEnvironmentForPost(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{
			Main: "index.js", Post: "post.js", PostCondition: testActionSite("env.FLAG == 'true' && github.token != ''"),
		}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{
		EnvironmentLayers: [][]Binding{
			{{Name: "FLAG", Value: testWorkflowSite("true")}},
			{{Name: "FLAG", Value: testWorkflowSite("false")}},
		},
		MainEnvironmentMutable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("post condition forgot the stable invocation environment")
	}
}

func TestInventoryActionAuthorityEvaluatesStableInvocationEnvironmentBeforeMainMutation(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{
			Main: "index.js", Post: "post.js", PostCondition: testActionSite("env.FLAG == 'true' && github.token != ''"),
		}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{
		EnvironmentLayers: [][]Binding{
			{{Name: "BASE", Value: testWorkflowSite("false")}},
			{{Name: "FLAG", Value: testWorkflowSite("${{ env.BASE }}")}},
		},
		MainEnvironmentMutable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("post condition forgot the invocation environment resolved before main ran")
	}
}

func TestInventoryActionAuthorityEvaluatesChildStableEnvironmentWithParentInputs(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "enabled"}},
			Composite: &CompositeAction{Steps: []CompositeStep{{
				Env:        []Binding{{Name: "FLAG", Value: testActionSite("${{ inputs.enabled }}")}},
				Invocation: &Invocation{Lock: "child", Uses: testActionSite("./child")},
			}}},
		},
		"child": {Source: "workspace", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{
			Main: "index.js", Post: "post.js", PostCondition: testActionSite("env.FLAG == 'true' && github.token != ''"),
		}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "enabled", Value: testWorkflowSite("false")}},
	}, ActionAuthorityContext{MainEnvironmentMutable: true})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("child post condition did not evaluate stable environment with parent action inputs")
	}
}

func TestInventoryActionAuthorityResolvesChildInputsBeforeChildEnvironment(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{{
				Env:        []Binding{{Name: "FLAG", Value: testActionSite("false")}},
				Invocation: &Invocation{Lock: "child", Uses: testActionSite("./child"), With: []Binding{{Name: "enabled", Value: testActionSite("${{ env.FLAG }}")}}},
			}}},
		},
		"child": {
			Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "enabled"}},
			Composite: &CompositeAction{Steps: []CompositeStep{{
				Condition: testActionSite("inputs.enabled == 'true'"), Run: &Run{Command: testActionSite("echo ${{ github.token }}")},
			}}},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{Environment: map[string]string{"FLAG": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("child input used the child environment instead of the parent environment")
	}
}

func TestInventoryActionAuthorityRejectsInvalidCompositeConditionAfterAnalysisError(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{{
			Condition: testActionSite("secrets.UNSUPPORTED != ''"), Run: &Run{Command: testActionSite("echo ok")},
		}}}},
	}
	if _, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{}); err == nil {
		t.Fatal("invalid composite condition passed token-only fallback")
	}
}

func TestWorkflowEnvironmentMutableBeforeSkipsKnownFalseSteps(t *testing.T) {
	steps := []Step{
		{Condition: testWorkflowSite("matrix.enabled"), Run: &Run{Command: testWorkflowSite("echo skipped")}},
		{Run: &Run{Command: testWorkflowSite("echo target")}},
	}
	mutable, err := WorkflowEnvironmentMutableBefore(steps, 1, ActionAuthorityContext{Matrix: map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	if mutable {
		t.Fatal("known-false earlier workflow step made environment mutable")
	}
}

func TestWorkflowEnvironmentMutableBeforeUsesStepEnvironmentAndBackgroundBarriers(t *testing.T) {
	jobEnv := []Binding{{Name: "ENABLED", Value: testWorkflowSite("false")}}
	stepEnv := []Binding{{Name: "ENABLED", Value: testWorkflowSite("true")}}
	foreground := []Step{
		{Env: stepEnv, Condition: testWorkflowSite("env.ENABLED == 'true'"), Run: &Run{Command: testWorkflowSite("echo foreground")}},
		{Run: &Run{Command: testWorkflowSite("echo target")}},
	}
	mutable, err := WorkflowEnvironmentMutableBefore(foreground, 1, ActionAuthorityContext{EnvironmentLayers: [][]Binding{jobEnv}})
	if err != nil {
		t.Fatal(err)
	}
	if !mutable {
		t.Fatal("step environment did not make an earlier foreground step reachable")
	}

	background := []Step{
		{ID: "background", Background: true, Run: &Run{Command: testWorkflowSite("echo background")}},
		{Run: &Run{Command: testWorkflowSite("echo target")}},
	}
	mutable, err = WorkflowEnvironmentMutableBefore(background, 1, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if mutable {
		t.Fatal("unawaited background step made environment immediately mutable")
	}
	barrier := []Step{
		background[0],
		{Kind: "wait", Targets: []string{"background"}},
		{Run: &Run{Command: testWorkflowSite("echo target")}},
	}
	mutable, err = WorkflowEnvironmentMutableBefore(barrier, 2, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !mutable {
		t.Fatal("background barrier did not make committed environment mutable")
	}
	waitAll := []Step{
		{ID: "Mixed-Case", Background: true, Run: &Run{Command: testWorkflowSite("echo background")}},
		{Kind: "wait-all"},
		{Run: &Run{Command: testWorkflowSite("echo target")}},
	}
	mutable, err = WorkflowEnvironmentMutableBefore(waitAll, 2, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !mutable {
		t.Fatal("wait-all did not make committed background environment mutable")
	}
	caseInsensitiveTarget := []Step{
		waitAll[0],
		{Kind: "wait", Targets: []string{"mixed-case"}},
		waitAll[2],
	}
	mutable, err = WorkflowEnvironmentMutableBefore(caseInsensitiveTarget, 2, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !mutable {
		t.Fatal("case-insensitive background target did not commit mutable environment")
	}
}

func TestInventoryActionAuthorityRetainsPreparationEnvironmentAfterCompositeWithoutPre(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "github", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{
				{Invocation: &Invocation{Lock: "composite", Uses: testActionSite("owner/composite@v1")}},
				{Invocation: &Invocation{Lock: "pre", Uses: testActionSite("owner/pre@v1")}},
			}},
		},
		"composite": {Source: "github", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{}},
		"pre": {
			Source: "github", Runtime: ActionRuntimeJavaScript,
			JavaScript: &JavaScriptAction{Pre: "pre.js", PreCondition: testActionSite("env.FLAG == 'true' && github.token != ''"), Main: "index.js"},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{Environment: map[string]string{"FLAG": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("remote composite without a pre phase erased the preparation environment")
	}
}

func TestInventoryActionAuthorityKeepsRuntimeDependentSuppliedInputErrorsConservative(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "value"}}, Composite: &CompositeAction{}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "value", Value: testWorkflowSite("${{ inputs.enabled && fromJSON('bad') || '' }}")}},
	}, ActionAuthorityContext{UnknownWorkflowInputs: map[string]bool{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("runtime-dependent tokenless supplied input requested token authority")
	}
}

func TestInventoryActionAuthorityKeepsRuntimeDependentCompositeAnalysisConservative(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "enabled"}},
			Composite: &CompositeAction{Steps: []CompositeStep{{
				Condition: testActionSite("inputs.enabled && fromJSON('bad')"),
				Run:       &Run{Command: testActionSite("echo ${{ github.token }}")},
			}}},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "enabled", Value: testWorkflowSite("${{ inputs.enabled }}")}},
	}, ActionAuthorityContext{UnknownWorkflowInputs: map[string]bool{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("runtime-dependent composite condition did not keep token branch reachable")
	}
}

func TestInventoryActionAuthorityKeepsRuntimeDependentMetadataTokenConservative(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite, Inputs: []ActionInput{{Name: "enabled"}},
			Composite: &CompositeAction{Steps: []CompositeStep{{Run: &Run{
				Command: testActionSite("echo ${{ inputs.enabled && fromJSON('bad') && github.token || '' }}"),
			}}}},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{
		Lock: "root", Inputs: []Binding{{Name: "enabled", Value: testWorkflowSite("${{ inputs.enabled }}")}},
	}, ActionAuthorityContext{UnknownWorkflowInputs: map[string]bool{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("runtime-dependent metadata error did not retain conservative token authority")
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

func TestInventoryActionAuthorityIncludesCompositeConditionToken(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{{
			Condition: testActionSite("github.token != ''"), Run: &Run{Command: testActionSite("echo ok")},
		}}}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("composite condition did not grant GitHub token authority")
	}
}

func TestInventoryActionAuthorityIgnoresUnevaluatedCompositeName(t *testing.T) {
	actions := map[string]Action{
		"root": {Source: "workspace", Runtime: ActionRuntimeComposite, Composite: &CompositeAction{Steps: []CompositeStep{{
			Name: testActionSite("${{ github.token }}"), Run: &Run{Command: testActionSite("echo ok")},
		}}}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("unevaluated composite name granted GitHub token authority")
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

func TestInventoryActionAuthorityForgetsNestedPreparationEnvironmentAfterEveryPre(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "github", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{
				{Invocation: &Invocation{Lock: "first", Uses: testActionSite("owner/first@v1")}},
				{Env: []Binding{{Name: "FLAG", Value: testActionSite("false")}}, Invocation: &Invocation{Lock: "nested", Uses: testActionSite("owner/nested@v1")}},
			}},
		},
		"first": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{Pre: "pre.js", Main: "index.js"}},
		"nested": {
			Source: "github", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{
				{Invocation: &Invocation{Lock: "mutator", Uses: testActionSite("owner/mutator@v1")}},
				{Invocation: &Invocation{Lock: "guarded", Uses: testActionSite("owner/guarded@v1")}},
			}},
		},
		"mutator": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{Pre: "pre.js", Main: "index.js"}},
		"guarded": {Source: "github", Runtime: ActionRuntimeJavaScript, JavaScript: &JavaScriptAction{
			Pre: "pre.js", PreCondition: testActionSite("env.FLAG == 'true' && github.token != ''"), Main: "index.js",
		}},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root", Condition: testWorkflowSite("false")}, ActionAuthorityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("an earlier nested pre did not make the later preparation environment conservative")
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

func TestInventoryActionAuthorityForgetsParentEnvironmentAfterChildAction(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{
				{Invocation: &Invocation{Lock: "child", Uses: testActionSite("./child")}},
				{Condition: testActionSite("env.FLAG == 'true'"), Run: &Run{Command: testActionSite("echo ${{ github.token }}")}},
			}},
		},
		"child": {
			Source: "workspace", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{{Run: &Run{Command: testActionSite("echo FLAG=true >> $GITHUB_ENV")}}}},
		},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{Environment: map[string]string{"FLAG": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	if !authority.GitHubToken {
		t.Fatal("a child action could not override the parent environment for a later sibling")
	}
}

func TestInventoryActionAuthorityRetainsEnvironmentAfterNativeChild(t *testing.T) {
	actions := map[string]Action{
		"root": {
			Source: "workspace", Runtime: ActionRuntimeComposite,
			Composite: &CompositeAction{Steps: []CompositeStep{
				{Invocation: &Invocation{Lock: "native", Uses: testActionSite("buildkite/native")}},
				{Condition: testActionSite("env.FLAG == 'true'"), Run: &Run{Command: testActionSite("echo ${{ github.token }}")}},
			}},
		},
		"native": {Source: "native", Runtime: ActionRuntimeNative},
	}
	authority, err := InventoryActionAuthority(actions, ActionInvocation{Lock: "root"}, ActionAuthorityContext{Environment: map[string]string{"FLAG": "false"}})
	if err != nil {
		t.Fatal(err)
	}
	if authority.GitHubToken {
		t.Fatal("native child discarded an environment value it cannot change")
	}
}

func testActionSite(source string) Site {
	return Site{Source: source, Provenance: ProvenanceAction}
}

func testWorkflowSite(source string) Site {
	return Site{Source: source, Provenance: ProvenanceWorkflow}
}
