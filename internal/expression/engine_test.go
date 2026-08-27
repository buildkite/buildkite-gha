package expression

import (
	"reflect"
	"strings"
	"testing"
)

func TestEngineProfilesExerciseEveryOperation(t *testing.T) {
	engine := NewEngine()
	profiles := Profiles()
	if len(profiles) != len(profileIDs()) {
		t.Fatalf("profile table has %d entries, want %d", len(profiles), len(profileIDs()))
	}
	for _, id := range profileIDs() {
		t.Run(string(id), func(t *testing.T) {
			profile, ok := profiles[id]
			if !ok {
				t.Fatal("profile is missing")
			}
			result := ResultString
			if profile.Form == FormExpression {
				result = ResultBoolean
			}
			site := Site{Profile: id, Result: result, Purpose: PurposeExpression}
			if _, err := engine.Validate(site); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if _, err := engine.Evaluate(site, Values{}); err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if reduced, err := engine.Reduce(site, Values{}); err != nil || !reduced.Known {
				t.Fatalf("Reduce() = %#v, %v", reduced, err)
			}
			if analysis, err := engine.Analyze(site, AbstractValues{}); err != nil || !analysis.Value.Known {
				t.Fatalf("Analyze() = %#v, %v", analysis, err)
			}
		})
	}
}

func TestEngineMatchesLegacyRuntimeOracle(t *testing.T) {
	engine := NewEngine()
	runtime := Context{Env: map[string]string{"NAME": "world"}, Matrix: map[string]any{"enabled": true}}
	condition := ConditionContext{Env: runtime.Env, Matrix: runtime.Matrix}
	tests := []struct {
		name   string
		site   Site
		values Values
		legacy func() (any, error)
	}{
		{name: "job condition", site: Site{Source: "matrix.enabled", Profile: ProfileJobCondition, Result: ResultBoolean}, values: Values{Condition: condition}, legacy: func() (any, error) { return EvaluateCondition("matrix.enabled", condition) }},
		{name: "step template", site: Site{Source: "hello ${{ env.NAME }}", Profile: ProfileStepTemplate, Result: ResultString}, values: Values{Runtime: runtime}, legacy: func() (any, error) { return EvaluateStep("hello ${{ env.NAME }}", runtime) }},
		{name: "step control", site: Site{Source: "${{ matrix.enabled }}", Profile: ProfileStepControl, Result: ResultBoolean}, values: Values{Runtime: runtime}, legacy: func() (any, error) { return EvaluateStepControl("${{ matrix.enabled }}", runtime) }},
		{name: "runtime template", site: Site{Source: "hello ${{ env.NAME }}", Profile: ProfileRuntimeTemplate, Result: ResultString}, values: Values{Runtime: runtime}, legacy: func() (any, error) { return Evaluate("hello ${{ env.NAME }}", runtime) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, gotErr := engine.Evaluate(test.site, test.values)
			want, wantErr := test.legacy()
			if !reflect.DeepEqual(got, want) || errorText(gotErr) != errorText(wantErr) {
				t.Fatalf("Engine.Evaluate() = %#v, %v; legacy = %#v, %v", got, gotErr, want, wantErr)
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
		case TokenDirect, TokenWorkflowContext, TokenCompositeContext:
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

func profileIDs() []ProfileID {
	return []ProfileID{
		ProfileCompile, ProfileCompileTemplate, ProfilePartialTemplate, ProfileCompileJobCondition, ProfileCompileStepCondition, ProfileCompileCallCondition,
		ProfileReusableInput, ProfileRunName, ProfileJobCondition, ProfileStepCondition, ProfileCallCondition,
		ProfileActionLifecycle, ProfileJobEnvironment, ProfileJobDefault, ProfileJobOutput, ProfileStepTemplate,
		ProfileStepControl, ProfileRuntimeTemplate, ProfileServiceTemplate, ProfileServiceCredential, ProfileServiceMap,
		ProfileActionInputDefault, ProfileDockerActionArg,
	}
}
