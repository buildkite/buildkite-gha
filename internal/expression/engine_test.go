package expression

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEngineLiteralPreservesExpressionTypes(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{nil, "null"},
		{true, "true"},
		{"yes please", "'yes please'"},
		{"it's 10", "'it''s 10'"},
		{"10", "'10'"},
		{json.Number("10.5"), "10.5"},
		{7, "7"},
		{2.5, "2.5"},
	}
	engine := NewEngine()
	for _, test := range tests {
		got, err := engine.Literal(test.value)
		if err != nil || got != test.want {
			t.Errorf("Literal(%#v) = %q, %v; want %q", test.value, got, err, test.want)
		}
	}
	if _, err := engine.Literal(map[string]any{"key": "value"}); err == nil {
		t.Fatal("Literal() accepted an aggregate")
	}
}

func TestEngineProfilesExerciseEveryOperation(t *testing.T) {
	engine := NewEngine()
	profiles := Profiles()
	if len(profiles) != len(profileIDs()) {
		t.Fatalf("profile table has %d entries, want %d", len(profiles), len(profileIDs()))
	}
	compile := CompileContext{
		GitHub: map[string]any{"event_name": "push", "server_url": "https://github.com"},
		Inputs: map[string]any{"enabled": true, "name": "value"},
		Matrix: map[string]any{"os": "linux"},
		Vars:   map[string]string{"NAME": "value"},
	}
	runtime := Context{
		Env:    map[string]string{"NAME": "value"},
		GitHub: map[string]any{"event_name": "push", "server_url": "https://github.com"},
		Inputs: map[string]string{"enabled": "true", "name": "value"},
		Needs:  map[string]NeedStatus{"build": {Outputs: map[string]string{"value": `{"name":"value"}`}, Result: "success"}},
	}
	condition := ConditionContext{
		Env:    runtime.Env,
		GitHub: runtime.GitHub,
		Inputs: map[string]any{"enabled": true, "name": "value"},
		Needs:  runtime.Needs,
	}
	values := Values{Compile: compile, Runtime: runtime, Condition: condition}
	abstract := AbstractValues{References: map[string]any{
		"env.name":                  "value",
		"github.event_name":         "push",
		"github.server_url":         "https://github.com",
		"inputs.enabled":            true,
		"inputs.name":               "value",
		"needs.build.outputs.value": `{"name":"value"}`,
	}}
	type example struct {
		source string
		result ResultType
		want   any
	}
	examples := map[ProfileID]example{
		ProfileCompile:               {"${{ case(true, contains('abc', 'b'), false) }}", ResultBoolean, true},
		ProfileCompileTemplate:       {"${{ case(true, true, false) }}", ResultString, "true"},
		ProfileCompileContainerImage: {"node:${{ case(true, 24, 25) }}", ResultString, "node:24"},
		ProfilePartialTemplate:       {"value-${{ inputs.name }}", ResultString, "value-value"},
		ProfileCompileJobCondition:   {"github.event_name == 'push'", ResultBoolean, true},
		ProfileCompileStepCondition:  {"inputs.enabled", ResultBoolean, true},
		ProfileCompileCallCondition:  {"inputs.enabled", ResultBoolean, true},
		ProfileReusableInput:         {"${{ 'value' }}", ResultString, "value"},
		ProfileRunName:               {"run-${{ github.event_name }}", ResultString, "run-push"},
		ProfileJobCondition:          {"always() && inputs.enabled", ResultBoolean, true},
		ProfileStepCondition:         {"always() && inputs.enabled", ResultBoolean, true},
		ProfileCallCondition:         {"always() && inputs.enabled", ResultBoolean, true},
		ProfileActionLifecycle:       {"${{ always() && inputs.enabled }}", ResultBoolean, true},
		ProfileJobEnvironment:        {"${{ inputs.name }}", ResultString, "value"},
		ProfileJobDefault:            {"${{ inputs.name }}", ResultString, "value"},
		ProfileJobOutput:             {"${{ inputs.name }}", ResultString, "value"},
		ProfileStepTemplate:          {"${{ case(true, format('{0}', inputs.name), 'unused') }}", ResultString, "value"},
		ProfileStepControl:           {"${{ true }}", ResultBoolean, true},
		ProfileRuntimeTemplate:       {"${{ env.NAME }}", ResultString, "value"},
		ProfileServiceTemplate:       {"${{ needs.build.outputs.value }}", ResultString, `{"name":"value"}`},
		ProfileServiceCredential:     {"${{ env.NAME }}", ResultString, "value"},
		ProfileServiceMap:            {"${{ fromJSON(needs.build.outputs.value) }}", ResultObject, []ObjectEntry{{Name: "name", Value: "value"}}},
		ProfileActionInputDefault:    {"${{ case(true, inputs.name, 'unused') }}", ResultString, "value"},
		ProfileDockerActionArg:       {"${{ inputs.name }}", ResultString, "value"},
	}
	for _, id := range profileIDs() {
		t.Run(string(id), func(t *testing.T) {
			if _, ok := profiles[id]; !ok {
				t.Fatal("profile is missing")
			}
			example, ok := examples[id]
			if !ok {
				t.Fatal("representative example is missing")
			}
			site := Site{Source: example.source, Profile: id, Result: example.result, Purpose: PurposeExpression}
			if _, err := engine.Validate(site); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if got, err := engine.Evaluate(site, values); err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			} else if !reflect.DeepEqual(got, example.want) {
				t.Fatalf("Evaluate() = %#v, want %#v", got, example.want)
			}
			if reduced, err := engine.Reduce(site, values); err != nil || (!reduced.Known && reduced.Source == "") {
				t.Fatalf("Reduce() = %#v, %v", reduced, err)
			}
			if analysis, err := engine.Analyze(site, abstract); err != nil {
				t.Fatalf("Analyze() = %#v, %v", analysis, err)
			}
		})
	}
}

func TestEngineCompileTemplateScalarAndContainerImageBoundaries(t *testing.T) {
	engine := NewEngine()
	for _, test := range []struct {
		source string
		want   string
	}{
		{source: "${{ true }}", want: "true"},
		{source: "${{ 2.5 }}", want: "2.5"},
		{source: "${{ null }}", want: ""},
	} {
		value, err := engine.Evaluate(Site{Source: test.source, Profile: ProfileCompileTemplate, Result: ResultString}, Values{})
		if err != nil || value != test.want {
			t.Errorf("Evaluate(%q) = %#v, %v; want %q", test.source, value, err, test.want)
		}
	}
	if _, err := engine.Evaluate(Site{Source: "${{ true }}", Profile: ProfileCompileContainerImage, Result: ResultString}, Values{}); err == nil || !strings.Contains(err.Error(), "want string") {
		t.Fatalf("strict complete expression error = %v", err)
	}
	value, err := engine.Evaluate(Site{Source: "node:${{ 24 }}", Profile: ProfileCompileContainerImage, Result: ResultString}, Values{})
	if err != nil || value != "node:24" {
		t.Fatalf("interpolated container image = %#v, %v", value, err)
	}
	_, err = engine.Evaluate(Site{Source: "${{ vars.IMAGE }}", Profile: ProfileCompileContainerImage, Result: ResultString}, Values{Compile: CompileContext{Vars: map[string]string{"IMAGE": "${{ secrets.TOKEN }}"}}})
	if err == nil || !strings.Contains(err.Error(), "contains expression syntax") {
		t.Fatalf("container image injection error = %v", err)
	}
}

func TestEngineMatchesUnexportedSemanticImplementations(t *testing.T) {
	engine := NewEngine()
	runtime := Context{Env: map[string]string{"NAME": "world"}, Matrix: map[string]any{"enabled": true}}
	condition := ConditionContext{Env: runtime.Env, Matrix: runtime.Matrix}
	tests := []struct {
		name   string
		site   Site
		values Values
		direct func() (any, error)
	}{
		{name: "job condition", site: Site{Source: "matrix.enabled", Profile: ProfileJobCondition, Result: ResultBoolean}, values: Values{Condition: condition}, direct: func() (any, error) { return EvaluateCondition("matrix.enabled", condition) }},
		{name: "step template", site: Site{Source: "hello ${{ env.NAME }}", Profile: ProfileStepTemplate, Result: ResultString}, values: Values{Runtime: runtime}, direct: func() (any, error) { return EvaluateStep("hello ${{ env.NAME }}", runtime) }},
		{name: "step control", site: Site{Source: "${{ matrix.enabled }}", Profile: ProfileStepControl, Result: ResultBoolean}, values: Values{Runtime: runtime}, direct: func() (any, error) { return EvaluateStepControl("${{ matrix.enabled }}", runtime) }},
		{name: "runtime template", site: Site{Source: "hello ${{ env.NAME }}", Profile: ProfileRuntimeTemplate, Result: ResultString}, values: Values{Runtime: runtime}, direct: func() (any, error) { return Evaluate("hello ${{ env.NAME }}", runtime) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, gotErr := engine.Evaluate(test.site, test.values)
			want, wantErr := test.direct()
			if !reflect.DeepEqual(got, want) || errorText(gotErr) != errorText(wantErr) {
				t.Fatalf("Engine.Evaluate() = %#v, %v; direct implementation = %#v, %v", got, gotErr, want, wantErr)
			}
		})
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestEngineProfileScopesAreDistinctAndAuthoritative(t *testing.T) {
	engine := NewEngine()
	validate := func(profile ProfileID, source string) error {
		_, err := engine.Validate(Site{Source: source, Profile: profile, Result: ResultBoolean, Purpose: PurposeExpression})
		return err
	}
	if err := validate(ProfileCompileJobCondition, "matrix.os == 'linux' && needs.build.result == 'success'"); err != nil {
		t.Fatalf("compile job condition: %v", err)
	}
	if err := validate(ProfileCompileJobCondition, "env.FLAG"); err == nil {
		t.Fatal("compile job condition admitted step env")
	}
	if err := validate(ProfileCompileStepCondition, "env.FLAG && steps.build.outcome == 'success' && matrix.os"); err != nil {
		t.Fatalf("compile step condition: %v", err)
	}
	if err := validate(ProfileCompileCallCondition, "needs.build.result == 'success' && inputs.enabled"); err != nil {
		t.Fatalf("compile call condition: %v", err)
	}
	if err := validate(ProfileCompileCallCondition, "matrix.os"); err == nil {
		t.Fatal("compile call condition admitted callee matrix")
	}

	_, err := engine.Validate(Site{Source: "${{ vars.NAME }}", Profile: ProfileRunName, Result: ResultString, Purpose: PurposeExpression})
	if err == nil || !strings.Contains(err.Error(), `run-name context "vars" is unavailable`) {
		t.Fatalf("run-name vars error = %v", err)
	}

	_, err = engine.Validate(Site{Source: "${{ unsupported.value }}", Profile: ProfilePartialTemplate, Result: ResultString, Purpose: PurposeExpression})
	if err == nil || !strings.Contains(err.Error(), `expression context "unsupported" is unavailable in this profile`) {
		t.Fatalf("declarative context policy error = %v", err)
	}
	_, err = engine.Validate(Site{Source: "${{ hashFiles('file') }}", Profile: ProfilePartialTemplate, Result: ResultString, Purpose: PurposeExpression})
	if err == nil || !strings.Contains(err.Error(), `expression function "hashFiles" is unavailable in this profile`) {
		t.Fatalf("declarative function policy error = %v", err)
	}
}

func TestEngineTemplateAnalysisExcludesKnownFalseTokenBranch(t *testing.T) {
	engine := NewEngine()
	site := Site{Source: "${{ false && github.token || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeWorkflowActionInput}
	analysis, err := engine.Analyze(site, AbstractValues{})
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.Value.Known || analysis.Value.Value != "" {
		t.Fatalf("Analyze() value = %#v, want known empty string", analysis.Value)
	}
	if analysis.Effects.GitHubToken != 0 {
		t.Fatalf("Analyze() token effects = %v, want none", analysis.Effects.GitHubToken)
	}

	site.Source = "${{ inputs.enabled && github.token || '' }}"
	analysis, err = engine.Analyze(site, AbstractValues{})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Value.Known || analysis.Effects.GitHubToken != GitHubTokenDirect {
		t.Fatalf("unknown Analyze() = %#v, want unknown direct-token effect", analysis)
	}
}

func TestEngineEventPayloadEffectsFollowLazyBranches(t *testing.T) {
	engine := NewEngine()
	tests := []struct {
		name       string
		site       Site
		references map[string]any
		values     Values
		wantValue  any
		wantEvent  bool
	}{
		{
			name:       "template known false",
			site:       Site{Source: "${{ inputs.enabled == 'yes' && github.event.action || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeExpression},
			references: map[string]any{"inputs.enabled": "no", "github.event.action": "opened"},
			values:     Values{Runtime: Context{Inputs: map[string]string{"enabled": "no"}, GitHub: map[string]any{"event": map[string]any{"action": "opened"}}}},
			wantValue:  "",
		},
		{
			name:       "template known true",
			site:       Site{Source: "${{ inputs.enabled == 'yes' && github.event.action || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeExpression},
			references: map[string]any{"inputs.enabled": "yes", "github.event.action": "opened"},
			values:     Values{Runtime: Context{Inputs: map[string]string{"enabled": "yes"}, GitHub: map[string]any{"event": map[string]any{"action": "opened"}}}},
			wantValue:  "opened",
			wantEvent:  true,
		},
		{
			name:       "condition known false",
			site:       Site{Source: "env.ENABLED == 'yes' && github.event.action == 'opened'", Profile: ProfileStepCondition, Result: ResultBoolean, Purpose: PurposeExpression},
			references: map[string]any{"env.enabled": "no", "github.event.action": "opened"},
			values:     Values{Condition: ConditionContext{Env: map[string]string{"ENABLED": "no"}, GitHub: map[string]any{"event": map[string]any{"action": "opened"}}}},
			wantValue:  false,
		},
		{
			name:       "condition known true",
			site:       Site{Source: "env.ENABLED == 'yes' && github.event.action == 'opened'", Profile: ProfileStepCondition, Result: ResultBoolean, Purpose: PurposeExpression},
			references: map[string]any{"env.enabled": "yes", "github.event.action": "opened"},
			values:     Values{Condition: ConditionContext{Env: map[string]string{"ENABLED": "yes"}, GitHub: map[string]any{"event": map[string]any{"action": "opened"}}}},
			wantValue:  true,
			wantEvent:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broad, err := engine.Analyze(test.site, AbstractValues{})
			if err != nil {
				t.Fatal(err)
			}
			if !broad.Effects.EventPayload {
				t.Fatal("unknown lazy branch did not conservatively retain event-payload authority")
			}
			narrow, err := engine.Analyze(test.site, AbstractValues{References: test.references})
			if err != nil {
				t.Fatal(err)
			}
			if narrow.Effects.EventPayload != test.wantEvent {
				t.Fatalf("event-payload effect = %v, want %v", narrow.Effects.EventPayload, test.wantEvent)
			}
			concrete, err := engine.Evaluate(test.site, test.values)
			if err != nil {
				t.Fatal(err)
			}
			if !narrow.Value.Known || !reflect.DeepEqual(narrow.Value.Value, concrete) || !reflect.DeepEqual(concrete, test.wantValue) {
				t.Fatalf("known analysis = %#v, concrete = %#v, want %#v", narrow.Value, concrete, test.wantValue)
			}
		})
	}
}

func FuzzEngineEventPayloadEffectsMatchConcreteReachability(f *testing.F) {
	f.Add("yes", "opened")
	f.Add("no", "closed")
	engine := NewEngine()
	site := Site{Source: "${{ inputs.enabled == 'yes' && github.event.action || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeExpression}
	broad, err := engine.Analyze(site, AbstractValues{})
	if err != nil {
		f.Fatal(err)
	}
	if !broad.Effects.EventPayload {
		f.Fatal("unknown lazy branch did not retain event-payload authority")
	}
	f.Fuzz(func(t *testing.T, enabled, action string) {
		references := map[string]any{"inputs.enabled": enabled, "github.event.action": action}
		narrow, err := engine.Analyze(site, AbstractValues{References: references})
		if err != nil {
			t.Fatal(err)
		}
		wantEvent := enabled == "yes"
		if narrow.Effects.EventPayload != wantEvent || narrow.Effects.EventPayload && !broad.Effects.EventPayload {
			t.Fatalf("narrow event effect = %v, broad = %v, want %v", narrow.Effects.EventPayload, broad.Effects.EventPayload, wantEvent)
		}
		concrete, err := engine.Evaluate(site, Values{Runtime: Context{
			Inputs: map[string]string{"enabled": enabled},
			GitHub: map[string]any{"event": map[string]any{"action": action}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !narrow.Value.Known || !reflect.DeepEqual(narrow.Value.Value, concrete) {
			t.Fatalf("known analysis = %#v, concrete = %#v", narrow.Value, concrete)
		}
	})
}

func TestEngineConditionAnalysisResolvesWholeKnownRoots(t *testing.T) {
	engine := NewEngine()
	tests := []struct {
		name       string
		source     string
		references map[string]any
		condition  ConditionContext
	}{
		{
			name:       "env",
			source:     `env.FLAG == 'yes'`,
			references: map[string]any{"env": map[string]any{"FLAG": "yes"}},
			condition:  ConditionContext{Env: map[string]string{"FLAG": "yes"}},
		},
		{
			name:       "vars",
			source:     `vars.FLAG == 'yes'`,
			references: map[string]any{"vars": map[string]any{"FLAG": "yes"}},
			condition:  ConditionContext{Vars: map[string]string{"FLAG": "yes"}},
		},
		{
			name:       "needs",
			source:     `needs.build.outputs.ready == 'yes'`,
			references: map[string]any{"needs": map[string]any{"build": map[string]any{"outputs": map[string]any{"ready": "yes"}, "result": "success"}}},
			condition:  ConditionContext{Needs: map[string]NeedStatus{"build": {Outputs: map[string]string{"ready": "yes"}, Result: "success"}}},
		},
		{
			name:       "steps",
			source:     `steps.build.outputs.ready == 'yes'`,
			references: map[string]any{"steps": map[string]any{"build": map[string]any{"conclusion": "success", "outcome": "success", "outputs": map[string]any{"ready": "yes"}}}},
			condition:  ConditionContext{Steps: map[string]StepStatus{"build": {Conclusion: "success", Outcome: "success", Outputs: map[string]string{"ready": "yes"}}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			site := Site{Source: test.source, Profile: ProfileStepCondition, Result: ResultBoolean, Purpose: PurposeExpression}
			unknown, err := engine.Analyze(site, AbstractValues{})
			if err != nil {
				t.Fatal(err)
			}
			if unknown.Value.Known {
				t.Fatalf("analysis without %s = %#v, want unknown", test.name, unknown.Value)
			}
			analysis, err := engine.Analyze(site, AbstractValues{References: test.references})
			if err != nil {
				t.Fatal(err)
			}
			concrete, err := engine.Evaluate(site, Values{Condition: test.condition})
			if err != nil {
				t.Fatal(err)
			}
			if !analysis.Value.Known || !reflect.DeepEqual(analysis.Value.Value, concrete) {
				t.Fatalf("known analysis = %#v, concrete = %#v", analysis.Value, concrete)
			}
		})
	}
}

func TestEngineAbstractEvaluationNarrowsMonotonicallyToConcrete(t *testing.T) {
	engine := NewEngine()
	tests := []struct {
		name       string
		site       Site
		references map[string]any
		values     Values
	}{
		{
			name:       "runtime false branch",
			site:       Site{Source: "${{ env.FLAG == 'yes' && github.token || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeWorkflowActionInput},
			references: map[string]any{"env.flag": "no", "github.token": "ghs_scoped"},
			values:     Values{Runtime: Context{Env: map[string]string{"FLAG": "no"}, GitHub: map[string]any{"token": "ghs_scoped"}}},
		},
		{
			name:       "runtime true branch",
			site:       Site{Source: "${{ vars.ENABLED == 'yes' && github.token || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeWorkflowActionInput},
			references: map[string]any{"vars.enabled": "yes", "github.token": "ghs_scoped"},
			values:     Values{Runtime: Context{Vars: map[string]string{"ENABLED": "yes"}, GitHub: map[string]any{"token": "ghs_scoped"}}},
		},
		{
			name:       "known failure status",
			site:       Site{Source: "failure() && needs.build.result == 'failure'", Profile: ProfileStepCondition, Result: ResultBoolean, Purpose: PurposeExpression},
			references: map[string]any{"failure": true, "needs.build.result": "failure"},
			values:     Values{Condition: ConditionContext{Failure: true, Needs: map[string]NeedStatus{"build": {Result: "failure"}}}},
		},
		{
			name:       "known cancelled status",
			site:       Site{Source: "cancelled() && steps.build.outcome == 'cancelled'", Profile: ProfileStepCondition, Result: ResultBoolean, Purpose: PurposeExpression},
			references: map[string]any{"cancelled": true, "steps.build.outcome": "cancelled"},
			values:     Values{Condition: ConditionContext{Cancelled: true, Steps: map[string]StepStatus{"build": {Outcome: "cancelled"}}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broad, err := engine.Analyze(test.site, AbstractValues{})
			if err != nil {
				t.Fatal(err)
			}
			narrow, err := engine.Analyze(test.site, AbstractValues{References: test.references})
			if err != nil {
				t.Fatal(err)
			}
			if narrow.Effects.GitHubToken&^broad.Effects.GitHubToken != 0 || narrow.Effects.EventPayload && !broad.Effects.EventPayload {
				t.Fatalf("narrow effects %#v are not contained by broad effects %#v", narrow.Effects, broad.Effects)
			}
			concrete, err := engine.Evaluate(test.site, test.values)
			if err != nil {
				t.Fatal(err)
			}
			if !narrow.Value.Known || !reflect.DeepEqual(narrow.Value.Value, concrete) {
				t.Fatalf("known analysis = %#v, concrete = %#v", narrow.Value, concrete)
			}
		})
	}
}

func FuzzEngineAbstractRuntimeTemplateMatchesConcrete(f *testing.F) {
	f.Add("yes", "ghs_scoped")
	f.Add("no", "")
	engine := NewEngine()
	site := Site{Source: "${{ env.FLAG == 'yes' && github.token || '' }}", Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeWorkflowActionInput}
	broad, err := engine.Analyze(site, AbstractValues{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, flag, token string) {
		references := map[string]any{"env.flag": flag, "github.token": token}
		narrow, err := engine.Analyze(site, AbstractValues{References: references})
		if err != nil {
			t.Fatal(err)
		}
		if narrow.Effects.GitHubToken&^broad.Effects.GitHubToken != 0 {
			t.Fatalf("narrow effects %#v are not contained by broad effects %#v", narrow.Effects, broad.Effects)
		}
		concrete, err := engine.Evaluate(site, Values{Runtime: Context{Env: map[string]string{"FLAG": flag}, GitHub: map[string]any{"token": token}}})
		if err != nil {
			t.Fatal(err)
		}
		if !narrow.Value.Known || !reflect.DeepEqual(narrow.Value.Value, concrete) {
			t.Fatalf("known analysis = %#v, concrete = %#v", narrow.Value, concrete)
		}
	})
}

func FuzzEngineAbstractKnownTemplateShapesMatchConcrete(f *testing.F) {
	for _, source := range []string{
		"literal",
		"${{ env.FLAG }}",
		"prefix-${{ inputs.name }}-suffix",
		"${{ github.event_name == 'push' && inputs.name || env.FLAG }}",
		"${{ format('{0}-{1}', inputs.name, github.event_name) }}",
	} {
		f.Add(source)
	}
	engine := NewEngine()
	values := Values{Runtime: Context{
		Env:     map[string]string{"FLAG": "yes"},
		GitHub:  map[string]any{"event_name": "push", "token": "ghs_scoped"},
		Inputs:  map[string]string{"name": "value"},
		Secrets: map[string]string{"NAME": "secret"},
	}}
	abstract := AbstractValues{References: map[string]any{
		"env.flag":          "yes",
		"github.event_name": "push",
		"github.token":      "ghs_scoped",
		"inputs.name":       "value",
		"secrets.name":      "secret",
	}}
	f.Fuzz(func(t *testing.T, source string) {
		site := Site{Source: source, Profile: ProfileStepTemplate, Result: ResultString, Purpose: PurposeWorkflowActionInput}
		if _, err := engine.Validate(site); err != nil {
			return
		}
		concrete, err := engine.Evaluate(site, values)
		if err != nil {
			return
		}
		analysis, err := engine.Analyze(site, abstract)
		if err != nil {
			t.Fatal(err)
		}
		if analysis.Value.Known && !reflect.DeepEqual(analysis.Value.Value, concrete) {
			t.Fatalf("known analysis = %#v, concrete = %#v", analysis.Value, concrete)
		}
	})
}

func TestEngineProfileTokenDeclarationsMatchValidation(t *testing.T) {
	engine := NewEngine()
	for id, profile := range Profiles() {
		if !containsFold(profile.Contexts, "github") {
			continue
		}
		source := "github.token"
		result := ResultBoolean
		if profile.Form == FormTemplate {
			source = "${{ github.token }}"
			result = ResultString
		} else if profile.semantics == semanticsStepControl {
			source = "${{ github.token }}"
		}
		_, err := engine.Validate(Site{Source: source, Profile: id, Result: result, Purpose: PurposeExpression})
		switch profile.Token {
		case TokenDenied:
			if err == nil {
				t.Errorf("profile %q declares token denied but validates github.token", id)
			}
		case TokenDirect, TokenWorkflowContext:
			if err != nil {
				t.Errorf("profile %q declares direct token access but rejects github.token: %v", id, err)
			}
		}
	}
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func TestEngineCaseFunctionPolicyIsClosedByProfile(t *testing.T) {
	profiles := Profiles()
	for _, id := range []ProfileID{
		ProfileCompile, ProfileCompileTemplate, ProfileCompileContainerImage, ProfilePartialTemplate,
		ProfileCompileJobCondition, ProfileCompileStepCondition, ProfileCompileCallCondition,
		ProfileReusableInput, ProfileRunName, ProfileJobCondition, ProfileStepCondition,
		ProfileCallCondition, ProfileActionLifecycle, ProfileJobEnvironment, ProfileJobDefault,
		ProfileJobOutput, ProfileStepTemplate, ProfileStepControl, ProfileActionInputDefault,
	} {
		if !containsFold(profiles[id].Functions, "case") {
			t.Errorf("profile %q does not admit case", id)
		}
	}
	for _, id := range []ProfileID{ProfileRuntimeTemplate, ProfileServiceTemplate, ProfileServiceCredential, ProfileServiceMap, ProfileDockerActionArg} {
		if containsFold(profiles[id].Functions, "case") {
			t.Errorf("profile %q unexpectedly admits case", id)
		}
	}
}

func profileIDs() []ProfileID {
	return []ProfileID{
		ProfileCompile, ProfileCompileTemplate, ProfileCompileContainerImage, ProfilePartialTemplate, ProfileCompileJobCondition, ProfileCompileStepCondition, ProfileCompileCallCondition,
		ProfileReusableInput, ProfileRunName, ProfileJobCondition, ProfileStepCondition, ProfileCallCondition,
		ProfileActionLifecycle, ProfileJobEnvironment, ProfileJobDefault, ProfileJobOutput, ProfileStepTemplate,
		ProfileStepControl, ProfileRuntimeTemplate, ProfileServiceTemplate, ProfileServiceCredential, ProfileServiceMap,
		ProfileActionInputDefault, ProfileDockerActionArg,
	}
}
